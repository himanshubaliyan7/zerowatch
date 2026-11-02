package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAuthAndRateLimit(t *testing.T) {
	ks := NewKeyStore(map[string]uint32{"secret-key-0123456789": 7})
	rl := NewRateLimiter(1, 2)
	var gotTenant uint32
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant = Tenant(r.Context())
		w.WriteHeader(204)
	}), Observe(slog.New(slog.NewTextHandler(io.Discard, nil))), Auth(ks, func(r *http.Request) bool { return r.URL.Path == "/healthz" }), rl.Middleware(nil))

	do := func(path, hdr string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if hdr != "" {
			req.Header.Set("Authorization", hdr)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := do("/api/x", ""); rec.Code != 401 {
		t.Fatalf("want 401 got %d", rec.Code)
	} else {
		var env map[string]map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if env["error"]["code"] != "UNAUTHORIZED" || env["error"]["requestId"] == "" {
			t.Fatalf("envelope %s", rec.Body.String())
		}
	}
	if rec := do("/api/x", "Bearer wrong"); rec.Code != 401 {
		t.Fatal("wrong key")
	}
	if rec := do("/healthz", ""); rec.Code != 204 {
		t.Fatal("public path")
	}
	if rec := do("/api/x", "Bearer secret-key-0123456789"); rec.Code != 204 || gotTenant != 7 {
		t.Fatalf("auth ok %d %d", rec.Code, gotTenant)
	}
	// SigV4 access key id maps to API key
	if rec := do("/api/x", "AWS4-HMAC-SHA256 Credential=secret-key-0123456789/20260101/us-east-1/monitoring/aws4_request, Signature=x"); rec.Code != 204 {
		t.Fatalf("sigv4 %d", rec.Code)
	}
	if rec := do("/api/x", "Bearer secret-key-0123456789"); rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("expected rate limit, got %d", rec.Code)
	}
}

// Two keys behind one IP (a proxy, a NAT) must not share a bucket, and a bad
// credential must still be throttled even though it never passes auth.
func TestGuardLimitsPerKeyAndThrottlesAuthFailures(t *testing.T) {
	const a, b = "secret-key-aaaaaaaaaaaa", "secret-key-bbbbbbbbbbbb"
	ks := NewKeyStore(map[string]uint32{a: 1, b: 2})
	rl := NewRateLimiter(1, 2)
	now := time.Unix(0, 0)
	rl.now = func() time.Time { return now }
	h := rl.Guard(Auth(ks, nil), nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	do := func(key string) int {
		req := httptest.NewRequest("GET", "/api/x", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c1, c2, c3 := do(a), do(a), do(a); c1 != 204 || c2 != 204 || c3 != 429 {
		t.Fatalf("key a: %d %d %d, want 204 204 429", c1, c2, c3)
	}
	if c := do(b); c != 204 {
		t.Fatalf("key b shares key a's bucket: %d", c)
	}
	if c1, c2, c3 := do("bad-1"), do("bad-2"), do("bad-3"); c1 != 401 || c2 != 401 || c3 != 429 {
		t.Fatalf("bad keys: %d %d %d, want 401 401 429", c1, c2, c3)
	}
	now = now.Add(2 * time.Second)
	if c := do("bad-4"); c != 401 {
		t.Fatalf("failure budget should refill: %d", c)
	}
}

// Bad credentials sent at once must not all slip past the failure budget
// while auth is still deciding.
func TestGuardThrottlesConcurrentAuthFailures(t *testing.T) {
	rl := NewRateLimiter(0.001, 2)
	slowReject := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(100 * time.Millisecond)
			WriteError(w, r, Unauthorized("invalid API key"))
		})
	}
	h := rl.Guard(slowReject, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	const n = 20
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/x", nil))
			codes <- rec.Code
		}()
	}
	refused := 0
	for i := 0; i < n; i++ {
		if <-codes == 401 {
			refused++
		}
	}
	if refused != 2 {
		t.Fatalf("%d of %d concurrent bad credentials reached auth; the burst is 2", refused, n)
	}
}

func TestClientIPTrustsOnlyConfiguredProxies(t *testing.T) {
	proxies := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	cases := []struct {
		name, remote string
		xff          []string
		trusted      []netip.Prefix
		want         string
	}{
		{"no proxies configured ignores the header", "203.0.113.9:5000", []string{"1.2.3.4"}, nil, "203.0.113.9"},
		{"untrusted peer ignores the header", "203.0.113.9:5000", []string{"1.2.3.4"}, proxies, "203.0.113.9"},
		{"trusted peer gives the hop it saw", "10.0.0.2:5000", []string{"198.51.100.7"}, proxies, "198.51.100.7"},
		{"spoofed left entries are skipped", "10.0.0.2:5000", []string{"6.6.6.6, 198.51.100.7"}, proxies, "198.51.100.7"},
		{"chained trusted proxies are walked past", "10.0.0.2:5000", []string{"6.6.6.6, 198.51.100.7, 10.1.1.1"}, proxies, "198.51.100.7"},
		{"repeated headers count as one list", "10.0.0.2:5000", []string{"6.6.6.6", "198.51.100.7"}, proxies, "198.51.100.7"},
		{"garbage stops at the last vouched hop", "10.0.0.2:5000", []string{"198.51.100.7, not-an-ip"}, proxies, "10.0.0.2"},
		{"all hops trusted gives the leftmost", "10.0.0.2:5000", []string{"10.9.9.9, 10.1.1.1"}, proxies, "10.9.9.9"},
		{"ports and brackets are stripped", "[::1]:5000", []string{"[2001:db8::5]:443"}, proxies, "2001:db8::5"},
		{"no header from a trusted peer is the peer", "10.0.0.2:5000", nil, proxies, "10.0.0.2"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		for _, v := range c.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := ClientIP(r, c.trusted); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// A client that rotates X-Forwarded-For must still land in one failure bucket.
func TestGuardIgnoresSpoofedForwardedFor(t *testing.T) {
	rl := NewRateLimiter(0.001, 2)
	h := rl.Guard(Auth(NewKeyStore(map[string]uint32{"secret-key-aaaaaaaaaaaa": 1}), nil), nil)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	var codes []int
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("GET", "/api/x", nil)
		req.Header.Set("Authorization", "Bearer bad")
		req.Header.Set("X-Forwarded-For", "198.51.100."+strconv.Itoa(i))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
	}
	if codes[0] != 401 || codes[1] != 401 || codes[2] != 429 {
		t.Fatalf("codes %v, want 401 401 429", codes)
	}
}

func TestTokenBucketRefill(t *testing.T) {
	rl := NewRateLimiter(10, 1)
	now := time.Unix(0, 0)
	rl.now = func() time.Time { return now }
	if ok, _, _ := rl.Allow("a"); !ok {
		t.Fatal()
	}
	if ok, _, _ := rl.Allow("a"); ok {
		t.Fatal("should be empty")
	}
	now = now.Add(100 * time.Millisecond)
	if ok, _, _ := rl.Allow("a"); !ok {
		t.Fatal("should refill")
	}
}

func TestDecodeAndErrors(t *testing.T) {
	var v struct{ A int }
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"A":1,"B":2}`))
	if err := DecodeJSON(rec, req, &v, 100, true); err == nil {
		t.Fatal("strict should reject unknown")
	}
	req = httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat(" ", 200)+`{}`))
	var ae *APIError
	if err := DecodeJSON(rec, req, &v, 100, false); !errors.As(err, &ae) || ae.Status != 413 {
		t.Fatalf("want 413 got %v", err)
	}
	sentinel := errors.New("dom")
	RegisterMapper(func(err error) *APIError {
		if errors.Is(err, sentinel) {
			return NotFound("x")
		}
		return nil
	})
	rec = httptest.NewRecorder()
	WriteError(rec, httptest.NewRequest("GET", "/", nil), sentinel)
	if rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	rec = httptest.NewRecorder()
	WriteError(rec, httptest.NewRequest("GET", "/", nil), errors.New("db password leaked"))
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "password") {
		t.Fatal("internal errors must not leak")
	}
}
