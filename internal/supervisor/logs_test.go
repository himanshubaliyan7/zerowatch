package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/ingest"
	"github.com/himanshubaliyan7/zerowatch/internal/model"
)

func TestInferFormat(t *testing.T) {
	cases := map[string][]string{
		FormatJSON:   {`{"level":"info","msg":"a"}`, `{"level":"error","msg":"b"}`},
		FormatLogfmt: {`ts=1 level=info msg="hello world" user=1`, `level=warn msg=x k=v`},
		FormatNginx:  {`10.0.0.1 - - [12/Oct/2026:10:00:00 +0000] "GET /x HTTP/1.1" 500 12 "-" "curl/8"`},
		FormatSyslog: {`Oct 12 10:00:00 host sshd[123]: Accepted key`, `Oct 12 10:00:01 host cron: run`},
		FormatPlain:  {`just some text`, `another line`},
	}
	for want, lines := range cases {
		if g := InferFormat(lines); g.Format != want {
			t.Errorf("want %s got %+v", want, g)
		}
	}
}

func TestParseLine(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	ev := ParseLine(FormatNginx, `10.0.0.1 - - [12/Oct/2026:10:00:00 +0000] "GET /x HTTP/1.1" 503 12 "-" "curl/8"`, now)
	if ev.Level != "ERROR" || ev.Attributes["status"] != "503" || ev.Attributes["path"] != "/x" {
		t.Fatalf("nginx parse: %+v", ev)
	}
	ev = ParseLine(FormatJSON, `{"level":"warning","msg":"slow","ts":"2026-10-12T11:59:00Z","route":"/a"}`, now)
	if ev.Level != "WARN" || ev.Message != "slow" || ev.Attributes["route"] != "/a" || ev.Timestamp != now.Add(-time.Minute).UnixMilli() {
		t.Fatalf("json parse: %+v", ev)
	}
}

type memWriter struct{ evs []model.LogEvent }

func (m *memWriter) WriteLogs(_ context.Context, _ uint32, ls []model.LogEvent) (ingest.Result, error) {
	m.evs = append(m.evs, ls...)
	return ingest.Result{Accepted: len(ls)}, nil
}

func TestTailerFollowsAppends(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")
	if err := os.WriteFile(p, []byte("{\"level\":\"info\",\"msg\":\"one\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := DiscoverLogFiles([]string{dir}, 3, 10)
	if len(files) != 1 {
		t.Fatalf("discover: %v", files)
	}
	w := &memWriter{}
	tl := NewTailer(w, 1, "test")
	now := time.Now()
	tl.Track(files, now)
	if n := tl.Poll(context.Background(), now); n != 1 {
		t.Fatalf("initial poll shipped %d", n)
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString("{\"level\":\"error\",\"msg\":\"two\"}\npartial")
	f.Close()
	if n := tl.Poll(context.Background(), now); n != 1 || w.evs[1].Level != "ERROR" || w.evs[1].LogGroup != "/host"+p {
		t.Fatalf("append poll: n=%d evs=%+v", n, w.evs)
	}
}
