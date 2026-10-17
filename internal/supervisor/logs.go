package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/ingest"
	"github.com/himanshubaliyan7/zerowatch/internal/model"
)

// Tier 3: autonomous log harvesting with schema inference. The supervisor
// walks well-known roots, samples 20 lines per candidate file, infers the
// format with a confidence score and tails new lines into the pipeline.
// Low-confidence files are still shipped as "plain" (full-text searchable),
// matching the blueprint's "never discard data" fallback.

// Log formats recognised by InferFormat.
const (
	FormatJSON   = "json"
	FormatLogfmt = "logfmt"
	FormatNginx  = "nginx"
	FormatSyslog = "syslog"
	FormatPlain  = "plain"
)

// MinConfidence below which a file is treated as plain text.
const MinConfidence = 0.6

var (
	reLogfmtPair = regexp.MustCompile(`(?:^|\s)([A-Za-z_][\w.\-]*)=("(?:[^"\\]|\\.)*"|\S*)`)
	reNginx      = regexp.MustCompile(`^(\S+) \S+ (\S+) \[([^\]]+)\] "(\S+) (\S+)[^"]*" (\d{3}) (\d+|-)(?: "([^"]*)" "([^"]*)")?`)
	reSyslog3164 = regexp.MustCompile(`^([A-Z][a-z]{2} [ \d]\d \d{2}:\d{2}:\d{2}) (\S+) ([^:\[\s]+)(?:\[(\d+)\])?: ?(.*)$`)
	reSyslogISO  = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})) (\S+) ([^:\[\s]+)(?:\[(\d+)\])?: ?(.*)$`)
	reSyslog5424 = regexp.MustCompile(`^<\d{1,3}>\d? ?`)
)

// FormatGuess is the outcome of schema inference over a sample.
type FormatGuess struct {
	Format     string  `json:"format"`
	Confidence float64 `json:"confidence"`
	Sampled    int     `json:"sampled"`
}

func isJSONLine(l string) bool {
	return strings.HasPrefix(l, "{") && json.Valid([]byte(l))
}

func isLogfmtLine(l string) bool {
	return len(reLogfmtPair.FindAllStringIndex(l, 3)) >= 2 && !strings.HasPrefix(l, "{")
}

func isSyslogLine(l string) bool {
	return reSyslog3164.MatchString(l) || reSyslogISO.MatchString(l) || reSyslog5424.MatchString(l)
}

// InferFormat scores each candidate format by the fraction of non-empty
// sample lines it matches and returns the best one. Order of the candidates
// breaks ties toward the more structured format.
func InferFormat(lines []string) FormatGuess {
	var sample []string
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			sample = append(sample, l)
		}
	}
	if len(sample) == 0 {
		return FormatGuess{Format: FormatPlain}
	}
	type cand struct {
		name  string
		match func(string) bool
	}
	cands := []cand{
		{FormatJSON, isJSONLine},
		{FormatNginx, reNginx.MatchString},
		{FormatSyslog, isSyslogLine},
		{FormatLogfmt, isLogfmtLine},
	}
	best := FormatGuess{Format: FormatPlain, Sampled: len(sample)}
	for _, c := range cands {
		n := 0
		for _, l := range sample {
			if c.match(l) {
				n++
			}
		}
		if conf := float64(n) / float64(len(sample)); conf > best.Confidence {
			best.Format, best.Confidence = c.name, conf
		}
	}
	if best.Confidence < MinConfidence {
		best.Confidence = 1 - best.Confidence
		best.Format = FormatPlain
	}
	return best
}

// ParseLine applies a format's extraction rule. It never fails: unparsable
// lines degrade to a plain message with an inferred level.
func ParseLine(format, line string, now time.Time) model.LogEvent {
	ev := model.LogEvent{Timestamp: now.UnixMilli(), Message: line}
	attrs := map[string]string{}
	switch format {
	case FormatJSON:
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			for k, v := range m {
				s := anyString(v)
				switch strings.ToLower(k) {
				case "level", "severity", "lvl", "loglevel":
					ev.Level = normLevel(s)
				case "msg", "message":
					ev.Message = s
				case "time", "ts", "timestamp", "@timestamp":
					if t, ok := parseAnyTime(v); ok {
						ev.Timestamp = t
					}
				default:
					if len(attrs) < 32 {
						attrs[k] = truncate(s, 256)
					}
				}
			}
		}
	case FormatLogfmt:
		for _, m := range reLogfmtPair.FindAllStringSubmatch(line, 64) {
			k, v := m[1], strings.Trim(m[2], `"`)
			switch strings.ToLower(k) {
			case "level", "lvl", "severity":
				ev.Level = normLevel(v)
			case "msg", "message":
				ev.Message = v
			case "time", "ts", "t":
				if t, ok := parseAnyTime(v); ok {
					ev.Timestamp = t
				}
			default:
				if len(attrs) < 32 {
					attrs[k] = truncate(v, 256)
				}
			}
		}
	case FormatNginx:
		if m := reNginx.FindStringSubmatch(line); m != nil {
			attrs["client"], attrs["method"], attrs["path"], attrs["status"], attrs["bytes"] = m[1], m[4], m[5], m[6], m[7]
			if m[9] != "" {
				attrs["user_agent"] = truncate(m[9], 256)
			}
			if t, err := time.Parse("02/Jan/2006:15:04:05 -0700", m[3]); err == nil {
				ev.Timestamp = t.UnixMilli()
			}
			code, _ := strconv.Atoi(m[6])
			switch {
			case code >= 500:
				ev.Level = "ERROR"
			case code >= 400:
				ev.Level = "WARN"
			default:
				ev.Level = "INFO"
			}
		}
	case FormatSyslog:
		if m := reSyslog3164.FindStringSubmatch(line); m != nil {
			if t, err := time.ParseInLocation("Jan _2 15:04:05", m[1], time.Local); err == nil {
				t = t.AddDate(now.Year(), 0, 0)
				if t.After(now.Add(24 * time.Hour)) { // December lines read in January
					t = t.AddDate(-1, 0, 0)
				}
				ev.Timestamp = t.UnixMilli()
			}
			attrs["host"], attrs["program"], ev.Message = m[2], m[3], m[5]
			if m[4] != "" {
				attrs["pid"] = m[4]
			}
		} else if m := reSyslogISO.FindStringSubmatch(line); m != nil {
			if t, err := time.Parse(time.RFC3339Nano, m[1]); err == nil {
				ev.Timestamp = t.UnixMilli()
			}
			attrs["host"], attrs["program"], ev.Message = m[2], m[3], m[5]
			if m[4] != "" {
				attrs["pid"] = m[4]
			}
		}
	}
	if ev.Level == "" {
		ev.Level = ingest.InferLevel(ev.Message)
	}
	// Clamp timestamps that parsing put outside the accepted window.
	if d := now.UnixMilli() - ev.Timestamp; d > int64(13*24*time.Hour/time.Millisecond) || d < -int64(time.Hour/time.Millisecond) {
		ev.Timestamp = now.UnixMilli()
	}
	if len(attrs) > 0 {
		ev.Attributes = attrs
	}
	return ev
}

func anyString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func normLevel(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "TRACE", "DEBUG", "DBG":
		return "DEBUG"
	case "INFO", "INFORMATION", "NOTICE":
		return "INFO"
	case "WARN", "WARNING":
		return "WARN"
	case "ERROR", "ERR":
		return "ERROR"
	case "FATAL", "CRITICAL", "CRIT", "PANIC", "ALERT", "EMERG":
		return "FATAL"
	}
	return ""
}

func parseAnyTime(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		switch {
		case x > 1e17: // ns
			return int64(x / 1e6), true
		case x > 1e14: // us
			return int64(x / 1e3), true
		case x > 1e11: // ms
			return int64(x), true
		case x > 1e8: // s
			return int64(x * 1000), true
		}
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.000", "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
			if t, err := time.Parse(layout, x); err == nil {
				return t.UnixMilli(), true
			}
		}
		if f, err := strconv.ParseFloat(x, 64); err == nil {
			return parseAnyTime(f)
		}
	}
	return 0, false
}

// LogSource is a discovered file.
type LogSource struct {
	Path     string      `json:"path"`
	Size     int64       `json:"size"`
	Group    string      `json:"logGroup"`
	Guess    FormatGuess `json:"guess"`
	Offset   int64       `json:"offset"`
	Shipped  uint64      `json:"shipped"`
	LastSeen time.Time   `json:"lastSeen"`
	Error    string      `json:"error,omitempty"`
}

var skipSuffixes = []string{".gz", ".xz", ".zst", ".bz2", ".zip", ".old", ".journal", ".db", ".pid", ".lock"}
var skipNames = map[string]bool{"wtmp": true, "btmp": true, "lastlog": true, "faillog": true, "utmp": true}
var reRotated = regexp.MustCompile(`\.\d+$`)

// candidateLog decides whether a file looks like a live text log.
func candidateLog(path string, info fs.FileInfo) bool {
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return false
	}
	base := filepath.Base(path)
	if skipNames[base] || reRotated.MatchString(base) {
		return false
	}
	for _, s := range skipSuffixes {
		if strings.HasSuffix(base, s) {
			return false
		}
	}
	ext := filepath.Ext(base)
	return ext == ".log" || ext == "" || ext == ".txt" || ext == ".out" || ext == ".err"
}

// DiscoverLogFiles walks roots (bounded depth and count) for live text logs.
func DiscoverLogFiles(roots []string, maxDepth, maxFiles int) []string {
	var out []string
	for _, root := range roots {
		root = filepath.Clean(root)
		baseDepth := strings.Count(root, string(filepath.Separator))
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if len(out) >= maxFiles {
				return fs.SkipAll
			}
			if d.IsDir() {
				if strings.Count(p, string(filepath.Separator))-baseDepth >= maxDepth || d.Name() == "journal" {
					return fs.SkipDir
				}
				return nil
			}
			info, err := d.Info()
			if err != nil || !candidateLog(p, info) {
				return nil
			}
			if f, err := os.Open(p); err == nil { // readable?
				f.Close()
				out = append(out, p)
			}
			return nil
		})
	}
	sort.Strings(out)
	return out
}

// sampleTail returns up to n complete lines from the last 16 KiB of a file.
func sampleTail(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	const window = 16 << 10
	off := st.Size() - window
	if off < 0 {
		off = 0
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil, err
	}
	if !looksText(buf) {
		return nil, errBinary
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if off > 0 && len(lines) > 1 {
		lines = lines[1:] // first line is probably partial
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

type binaryErr struct{}

func (binaryErr) Error() string { return "binary file" }

var errBinary = binaryErr{}

func looksText(b []byte) bool {
	if len(b) > 4096 {
		b = b[:4096]
	}
	return bytes.IndexByte(b, 0) < 0
}

// LogWriter is the subset of the ingest pipeline the tailer needs.
type LogWriter interface {
	WriteLogs(ctx context.Context, tenant uint32, ls []model.LogEvent) (ingest.Result, error)
}

// Tailer ships new lines of discovered files. It starts each new file near
// its end (the last InitialBytes) so a first run shows recent history
// without re-ingesting gigabytes, follows appends, and restarts from zero
// on truncation/rotation.
type Tailer struct {
	Writer       LogWriter
	Tenant       uint32
	Stream       string
	InitialBytes int64
	MaxPerCycle  int64

	mu      sync.Mutex
	sources map[string]*LogSource
}

// NewTailer constructs a tailer.
func NewTailer(w LogWriter, tenant uint32, stream string) *Tailer {
	return &Tailer{Writer: w, Tenant: tenant, Stream: stream, InitialBytes: 32 << 10, MaxPerCycle: 512 << 10, sources: map[string]*LogSource{}}
}

// Track adds (or refreshes) files, inferring format for new ones.
func (t *Tailer) Track(paths []string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, p := range paths {
		if s, ok := t.sources[p]; ok {
			s.LastSeen = now
			continue
		}
		src := &LogSource{Path: p, Group: "/host" + p, LastSeen: now}
		lines, err := sampleTail(p, 20)
		if err != nil {
			continue // binary or unreadable: not a log
		}
		src.Guess = InferFormat(lines)
		if st, err := os.Stat(p); err == nil {
			src.Size = st.Size()
			src.Offset = max(0, st.Size()-t.InitialBytes)
		}
		t.sources[p] = src
	}
	// Forget files not seen for 10 minutes (deleted/rotated away).
	for p, s := range t.sources {
		if now.Sub(s.LastSeen) > 10*time.Minute {
			delete(t.sources, p)
		}
	}
}

// Sources returns a snapshot.
func (t *Tailer) Sources() []LogSource {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]LogSource, 0, len(t.sources))
	for _, s := range t.sources {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Poll reads appended data from every source and ships it. Returns lines shipped.
func (t *Tailer) Poll(ctx context.Context, now time.Time) int {
	t.mu.Lock()
	srcs := make([]*LogSource, 0, len(t.sources))
	for _, s := range t.sources {
		srcs = append(srcs, s)
	}
	t.mu.Unlock()
	total := 0
	for _, s := range srcs {
		if ctx.Err() != nil {
			break
		}
		evs, newOff, size, err := t.read(s, now)
		t.mu.Lock()
		s.Size = size
		if err != nil {
			s.Error = err.Error()
		} else {
			s.Error = ""
			s.Offset = newOff
		}
		t.mu.Unlock()
		for i := 0; i < len(evs); i += 1000 {
			batch := evs[i:min(i+1000, len(evs))]
			res, _ := t.Writer.WriteLogs(ctx, t.Tenant, batch)
			t.mu.Lock()
			s.Shipped += uint64(res.Accepted)
			t.mu.Unlock()
			total += res.Accepted
		}
	}
	return total
}

func (t *Tailer) read(s *LogSource, now time.Time) ([]model.LogEvent, int64, int64, error) {
	f, err := os.Open(s.Path)
	if err != nil {
		return nil, s.Offset, s.Size, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, s.Offset, s.Size, err
	}
	off := s.Offset
	if st.Size() < off { // truncated or replaced
		off = 0
	}
	if st.Size() == off {
		return nil, off, st.Size(), nil
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, off, st.Size(), err
	}
	r := bufio.NewReaderSize(io.LimitReader(f, t.MaxPerCycle), 64<<10)
	var evs []model.LogEvent
	consumed := off
	skipFirst := s.Offset > 0 && off == s.Offset && off == max(0, s.Size-t.InitialBytes) && s.Shipped == 0 && off > 0
	for {
		line, err := r.ReadString('\n')
		if err != nil { // partial trailing line: leave for next poll
			break
		}
		consumed += int64(len(line))
		if skipFirst { // initial seek likely landed mid-line
			skipFirst = false
			continue
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		if len(line) > 16<<10 {
			line = line[:16<<10]
		}
		ev := ParseLine(s.Guess.Format, line, now)
		ev.LogGroup, ev.LogStream = s.Group, t.Stream
		if ev.Attributes == nil {
			ev.Attributes = map[string]string{}
		}
		ev.Attributes["source"] = "file"
		ev.Attributes["format"] = s.Guess.Format
		evs = append(evs, ev)
	}
	return evs, consumed, st.Size(), nil
}
