//go:build linux && ebpf

package probe

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerowatch/zerowatch/internal/model"
)

// e2eHelperEnv makes the test binary act as the traced HTTP server
// (TestHelperHTTPServer) so it runs as a separate process with its own PID.
const e2eHelperEnv = "ZW_E2E_HELPER"

// Routes served by both helper servers: /boom is a 500, /missing a 404,
// /slow sleeps 30ms, everything else is a 200.
const pyServer = pyHandler + `
srv = http.server.HTTPServer(('127.0.0.1', 0), H)
print(srv.server_address[1], flush=True)
srv.serve_forever()
`

// pyHandler is the request handler shared by the plain and TLS Python servers.
const pyHandler = `
import http.server, sys, time
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(s):
        c = {'/boom': 500, '/missing': 404}.get(s.path, 200)
        if s.path == '/slow':
            time.sleep(0.03)
        b = b'ok'
        s.send_response(c)
        s.send_header('Content-Length', str(len(b)))
        s.end_headers()
        s.wfile.write(b)
    def log_message(s, *a):
        pass
`

// TestHelperHTTPServer is not a test: it is the Go HTTP server child of
// TestKernelREDEndToEnd (net/http uses read/write syscalls).
func TestHelperHTTPServer(t *testing.T) {
	if os.Getenv(e2eHelperEnv) != "1" {
		t.Skip("helper process for TestKernelREDEndToEnd")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(ln.Addr().(*net.TCPAddr).Port)
	_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/boom":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/missing":
			http.NotFound(w, r)
		case "/slow":
			time.Sleep(30 * time.Millisecond)
			_, _ = io.WriteString(w, "ok")
		default:
			_, _ = io.WriteString(w, "ok")
		}
	}))
}

// startServer runs cmd, reads the listening port from its first stdout line
// and registers cleanup.
func startServer(t *testing.T, cmd *exec.Cmd) (pid int, port uint16) {
	t.Helper()
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("%s: reading port: %v", cmd.Path, err)
	}
	p, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("%s: bad port line %q", cmd.Path, line)
	}
	return cmd.Process.Pid, uint16(p)
}

// redTotals sums the datums one service emitted, by metric and route.
type redTotals struct {
	requests, errors map[string]float64
	maxLatency       map[string]float64
}

func collect(ms []model.MetricDatum, service string) redTotals {
	r := redTotals{map[string]float64{}, map[string]float64{}, map[string]float64{}}
	for _, m := range ms {
		var route, svc string
		for _, d := range m.Dimensions {
			switch d.Name {
			case "Route":
				route = d.Value
			case "Service":
				svc = d.Value
			}
		}
		if svc != service || m.Value == nil {
			continue
		}
		v := *m.Value
		switch m.MetricName {
		case "Requests":
			r.requests[route] += v
		case "Errors":
			r.errors[route] += v
		case "Latency":
			if v > r.maxLatency[route] {
				r.maxLatency[route] = v
			}
		}
	}
	return r
}

func (r redTotals) total(m map[string]float64) (n float64) {
	for _, v := range m {
		n += v
	}
	return n
}

// TestKernelREDEndToEnd loads the real probe, points it at HTTP servers in
// child processes and checks that requests, errors and latency come out of
// the ring buffer as ZeroWatch/RED datums. Needs root (or CAP_BPF+CAP_PERFMON)
// and tracefs mounted at /sys/kernel/tracing; see ebpf/README.md.
func TestKernelREDEndToEnd(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root / CAP_BPF")
	}
	obj := objForTest(t)
	if !tracefsMounted() {
		t.Skip("tracefs not mounted: mount -t tracefs nodev /sys/kernel/tracing")
	}

	type server struct {
		name string
		pid  int
		port uint16
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestHelperHTTPServer$")
	helper.Env = append(os.Environ(), e2eHelperEnv+"=1")
	pid, port := startServer(t, helper)
	servers := []server{{"e2e-go", pid, port}} // read + write
	if py, err := exec.LookPath("python3"); err == nil {
		pid, port := startServer(t, exec.Command(py, "-c", pyServer))
		servers = append(servers, server{"e2e-python", pid, port}) // recvfrom + sendto
	} else {
		t.Log("python3 not found: recvfrom/sendto path not exercised")
	}

	kp := New(slog.New(slog.NewTextHandler(io.Discard, nil)), obj).(*kernelProbe)
	kp.flushEvery = 500 * time.Millisecond
	var ts []Target
	for _, s := range servers {
		ts = append(ts, Target{PID: s.pid, Port: s.port, Service: s.name})
	}
	kp.SetTargets(ts)

	var mu sync.Mutex
	var got []model.MetricDatum
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- kp.Run(ctx, func(_ context.Context, ms []model.MetricDatum) {
			mu.Lock()
			got = append(got, ms...)
			mu.Unlock()
		})
	}()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		st := kp.Status()
		if st.Error != "" {
			t.Fatalf("probe fell back to %s: %s", st.Mode, st.Error)
		}
		if st.Healthy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("probe never became healthy")
		}
		time.Sleep(50 * time.Millisecond)
	}

	const nUsers, nMissing, nBoom, nSlow = 20, 5, 4, 3
	client := &http.Client{Timeout: 5 * time.Second}
	get := func(port uint16, path string) {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	for _, s := range servers {
		for i := 0; i < nUsers; i++ {
			get(s.port, "/users/"+strconv.Itoa(i))
		}
		for i := 0; i < nMissing; i++ {
			get(s.port, "/missing")
		}
		for i := 0; i < nBoom; i++ {
			get(s.port, "/boom")
		}
		for i := 0; i < nSlow; i++ {
			get(s.port, "/slow")
		}
	}

	want := float64(nUsers + nMissing + nBoom + nSlow)
	deadline = time.Now().Add(15 * time.Second)
	for _, s := range servers {
		var r redTotals
		for {
			mu.Lock()
			r = collect(got, s.name)
			mu.Unlock()
			if r.total(r.requests) >= want || time.Now().After(deadline) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("%s (pid %d): requests=%v errors=%v maxLatencyMs=%v", s.name, s.pid, r.requests, r.errors, r.maxLatency)
		if n := r.total(r.requests); n != want {
			t.Errorf("%s: %v requests, want %v", s.name, n, want)
		}
		checks := []struct {
			name      string
			got, want float64
		}{
			{"requests GET /users/:id", r.requests["GET /users/:id"], nUsers},
			{"requests GET /missing (404)", r.requests["GET /missing"], nMissing},
			{"requests GET /boom (500)", r.requests["GET /boom"], nBoom},
			{"errors GET /boom", r.errors["GET /boom"], nBoom},
			{"errors total (only 5xx)", r.total(r.errors), nBoom},
		}
		for _, c := range checks {
			if c.got != c.want {
				t.Errorf("%s: %s = %v, want %v", s.name, c.name, c.got, c.want)
			}
		}
		if l := r.maxLatency["GET /slow"]; l < 30 || l > 5000 {
			t.Errorf("%s: GET /slow max latency %vms, want >= 30ms", s.name, l)
		}
	}
	if st := kp.Status(); st.Mode != "ebpf" || st.Events == 0 {
		t.Errorf("status %+v, want mode ebpf with events", st)
	}
}

func tracefsMounted() bool {
	for _, p := range []string{"/sys/kernel/tracing/events/syscalls", "/sys/kernel/debug/tracing/events/syscalls"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}
