//go:build linux && ebpf

package probe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/model"
)

// pyTLSServer is pyHandler behind ssl.SSLContext.wrap_socket. CPython's _ssl
// links libssl.so.3 and uses SSL_read_ex/SSL_write_ex.
const pyTLSServer = pyHandler + `
import ssl
srv = http.server.HTTPServer(('127.0.0.1', 0), H)
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(sys.argv[1], sys.argv[2])
srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
print(srv.server_address[1], flush=True)
srv.serve_forever()
`

// nginxTLSConf is a single-process nginx (so its PID is the one serving)
// with the same routes except /slow. nginx uses SSL_read/SSL_write.
const nginxTLSConf = `
daemon off;
master_process off;
error_log stderr warn;
pid %[1]s/nginx.pid;
events { worker_connections 64; }
http {
    access_log off;
    client_body_temp_path %[1]s/body;
    proxy_temp_path %[1]s/proxy;
    fastcgi_temp_path %[1]s/fastcgi;
    uwsgi_temp_path %[1]s/uwsgi;
    scgi_temp_path %[1]s/scgi;
    server {
        listen 127.0.0.1:%[2]d ssl;
        ssl_certificate %[1]s/cert.pem;
        ssl_certificate_key %[1]s/key.pem;
        location /boom { return 500 "boom"; }
        location /missing { return 404 "no"; }
        location / { return 200 "ok"; }
    }
}
`

// writeSelfSigned writes cert.pem and key.pem for 127.0.0.1 into dir.
func writeSelfSigned(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "zerowatch-e2e"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// startNginx runs nginx in the foreground on a free port and waits until it
// accepts connections.
func startNginx(t *testing.T, bin, dir string) (pid int, port uint16) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port = uint16(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()
	conf := filepath.Join(dir, "nginx.conf")
	if err := os.WriteFile(conf, []byte(fmt.Sprintf(nginxTLSConf, dir, port)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-p", dir, "-c", conf, "-e", "stderr")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			c.Close()
			return cmd.Process.Pid, port
		}
		if time.Now().After(deadline) {
			t.Fatalf("nginx never listened on %d: %v", port, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestKernelTLSEndToEnd checks that HTTPS requests to OpenSSL-linked servers
// come out as ZeroWatch/RED datums via the SSL_read/SSL_write uprobes, with
// exact counts: the syscall path only sees ciphertext, so every event must
// come from the TLS path. Covers SSL_read_ex/SSL_write_ex (Python) and, if
// nginx is installed, SSL_read/SSL_write with keep-alive (many requests per
// SSL*). Clients: Go crypto/tls (not traced itself: it is the client and it
// does not use OpenSSL) and, if installed, curl -k (OpenSSL client, also not
// a target). Needs root and tracefs, like TestKernelREDEndToEnd.
func TestKernelTLSEndToEnd(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root / CAP_BPF")
	}
	obj := objForTest(t)
	if !tracefsMounted() {
		t.Skip("tracefs not mounted: mount -t tracefs nodev /sys/kernel/tracing")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	dir := t.TempDir()
	certPath, keyPath := writeSelfSigned(t, dir)

	type server struct {
		name string
		pid  int
		port uint16
		slow bool // serves /slow (30 ms)
	}
	pid, port := startServer(t, exec.Command(py, "-c", pyTLSServer, certPath, keyPath))
	servers := []server{{"e2e-python-tls", pid, port, true}}
	nginx, err := exec.LookPath("nginx")
	if err != nil {
		if _, e := os.Stat("/usr/sbin/nginx"); e == nil {
			nginx, err = "/usr/sbin/nginx", nil
		}
	}
	if err == nil {
		pid, port := startNginx(t, nginx, dir)
		servers = append(servers, server{"e2e-nginx-tls", pid, port, false})
	} else {
		t.Log("nginx not found: SSL_read/SSL_write (non-_ex) path not exercised")
	}
	curl, curlErr := exec.LookPath("curl")
	if curlErr != nil {
		t.Log("curl not found: OpenSSL client not exercised")
	}

	kp := New(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})), obj).(*kernelProbe)
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
		ready := st.Healthy
		for _, s := range servers {
			ready = ready && kp.tlsAttached(uint32(s.pid))
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("TLS uprobes never attached to all servers: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}

	const nUsers, nMissing, nBoom, nSlow, nCurl = 20, 5, 4, 3, 3
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // self-signed; also keeps HTTP/1.1 (no h2)
	}}
	get := func(port uint16, path string) {
		resp, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d%s", port, path))
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	var wantTotal float64
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
		if s.slow {
			for i := 0; i < nSlow; i++ {
				get(s.port, "/slow")
			}
		}
		if curlErr == nil {
			for i := 0; i < nCurl; i++ {
				out, err := exec.Command(curl, "-sk", "--http1.1", "-o", "/dev/null", "-w", "%{http_code}",
					fmt.Sprintf("https://127.0.0.1:%d/curl/%d", s.port, i)).Output()
				if err != nil || string(out) != "200" {
					t.Fatalf("curl %s: %q %v", s.name, out, err)
				}
			}
		}
	}

	for _, s := range servers {
		want := float64(nUsers + nMissing + nBoom)
		if s.slow {
			want += nSlow
		}
		if curlErr == nil {
			want += nCurl
		}
		wantTotal += want
		deadline := time.Now().Add(15 * time.Second)
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
		if curlErr == nil {
			checks = append(checks, struct {
				name      string
				got, want float64
			}{"requests GET /curl/:id", r.requests["GET /curl/:id"], nCurl})
		}
		for _, c := range checks {
			if c.got != c.want {
				t.Errorf("%s: %s = %v, want %v", s.name, c.name, c.got, c.want)
			}
		}
		if s.slow {
			if l := r.maxLatency["GET /slow"]; l < 30 || l > 5000 {
				t.Errorf("%s: GET /slow max latency %vms, want >= 30ms", s.name, l)
			}
		}
	}
	st := kp.Status()
	t.Logf("status: events=%d tlsEvents=%d tlsTargets=%d", st.Events, st.TLSEvents, st.TLSTargets)
	if st.TLSEvents != uint64(wantTotal) || st.Events != st.TLSEvents {
		t.Errorf("events=%d tlsEvents=%d, want both %v (every event from the TLS path)", st.Events, st.TLSEvents, wantTotal)
	}
	if st.TLSTargets != len(servers) {
		t.Errorf("tlsTargets=%d, want %d", st.TLSTargets, len(servers))
	}

	// Detach: dropping the targets must close the uprobes.
	kp.SetTargets(nil)
	deadline = time.Now().Add(5 * time.Second)
	for kp.Status().TLSTargets != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("uprobes still attached after targets removed: %+v", kp.Status())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
