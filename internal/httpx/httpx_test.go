package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
