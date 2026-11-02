package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// scriptedWriter returns errs[n] for the nth call, keeping the batch on success.
type scriptedWriter struct {
	errs  []error
	calls int
	evs   []model.LogEvent
}

func (s *scriptedWriter) WriteLogs(_ context.Context, _ uint32, ls []model.LogEvent) (ingest.Result, error) {
	n := s.calls
	s.calls++
	if n < len(s.errs) && s.errs[n] != nil {
		return ingest.Result{}, s.errs[n]
	}
	s.evs = append(s.evs, ls...)
	return ingest.Result{Accepted: len(ls)}, nil
}

func tailFile(t *testing.T, content string, w LogWriter) (*Tailer, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := NewTailer(w, 1, "test")
	tl.InitialBytes = 1 << 20
	tl.Track([]string{p}, time.Now())
	return tl, p
}

// A transient write failure must not lose lines: they are re-read next poll.
func TestTailerRetriesFailedWrite(t *testing.T) {
	w := &scriptedWriter{errs: []error{errors.New("backpressure")}}
	tl, _ := tailFile(t, "one\ntwo\n", w)
	now := time.Now()
	if n := tl.Poll(context.Background(), now); n != 0 {
		t.Fatalf("failed poll shipped %d", n)
	}
	if src := tl.Sources()[0]; src.Offset != 0 || !strings.Contains(src.Error, "backpressure") {
		t.Fatalf("offset %d error %q", src.Offset, src.Error)
	}
	if n := tl.Poll(context.Background(), now); n != 2 || len(w.evs) != 2 {
		t.Fatalf("retry shipped %d, have %d", n, len(w.evs))
	}
	if n := tl.Poll(context.Background(), now); n != 0 || len(w.evs) != 2 {
		t.Fatalf("lines shipped twice: %d, have %d", n, len(w.evs))
	}
}

// When a later batch fails, only that batch is retried: the earlier one was
// accepted and must not be shipped again.
func TestTailerKeepsProgressAcrossBatches(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 1500; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	w := &scriptedWriter{errs: []error{nil, errors.New("backpressure")}}
	tl, _ := tailFile(t, b.String(), w)
	now := time.Now()
	if n := tl.Poll(context.Background(), now); n != 1000 {
		t.Fatalf("first poll shipped %d, want 1000", n)
	}
	if n := tl.Poll(context.Background(), now); n != 500 || len(w.evs) != 1500 {
		t.Fatalf("second poll shipped %d, have %d", n, len(w.evs))
	}
	if !strings.Contains(w.evs[1000].Message, "line 1000") {
		t.Fatalf("resumed at %q", w.evs[1000].Message)
	}
}

// A batch that can never succeed is skipped, or the file would stall forever.
func TestTailerSkipsPermanentlyRejectedBatch(t *testing.T) {
	w := &scriptedWriter{errs: []error{ingest.ErrAllRejected}}
	tl, p := tailFile(t, "bad\n", w)
	now := time.Now()
	tl.Poll(context.Background(), now)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("good\n")
	f.Close()
	if n := tl.Poll(context.Background(), now); n != 1 || w.evs[0].Message != "good" {
		t.Fatalf("shipped %d: %+v", n, w.evs)
	}
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
