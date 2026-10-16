// Package httpx holds HTTP plumbing shared by the API gateway and the ingest
// listeners: the JSON error envelope, request decoding with size limits,
// tenant context, API-key auth, per-client token-bucket rate limiting,
// request IDs, access logging, panic recovery and security headers.
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// APIError is the structured error returned to clients:
//
//	{"error":{"code":"VALIDATION_ERROR","message":"...","requestId":"...","details":...}}
type APIError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
	Err     error  `json:"-"`
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }
func (e *APIError) Unwrap() error { return e.Err }

// Error constructors.
func BadRequest(msg string, details ...any) *APIError {
	return &APIError{Status: 400, Code: "VALIDATION_ERROR", Message: msg, Details: first(details)}
}
func NotFound(msg string) *APIError { return &APIError{Status: 404, Code: "NOT_FOUND", Message: msg} }
func Conflict(msg string) *APIError { return &APIError{Status: 409, Code: "CONFLICT", Message: msg} }
func Unauthorized(msg string) *APIError {
	return &APIError{Status: 401, Code: "UNAUTHORIZED", Message: msg}
}
func TooLarge(msg string) *APIError {
	return &APIError{Status: 413, Code: "PAYLOAD_TOO_LARGE", Message: msg}
}
func Unavailable(msg string) *APIError {
	return &APIError{Status: 503, Code: "SERVICE_UNAVAILABLE", Message: msg}
}

func first(d []any) any {
	if len(d) > 0 {
		return d[0]
	}
	return nil
}

// Mapper converts domain errors into APIErrors; registered by the composition root
// so httpx stays free of domain imports.
type Mapper func(error) *APIError

var mappers []Mapper

// RegisterMapper adds a domain error mapper (call at init/startup only).
func RegisterMapper(m Mapper) { mappers = append(mappers, m) }

type ctxKey int

const (
	tenantKey ctxKey = iota
	requestIDKey
	clientKey
)

// WithTenant stores the authenticated tenant.
func WithTenant(ctx context.Context, t uint32) context.Context {
	return context.WithValue(ctx, tenantKey, t)
}

// Tenant returns the authenticated tenant (1 when auth is disabled).
func Tenant(ctx context.Context) uint32 {
	if t, ok := ctx.Value(tenantKey).(uint32); ok {
		return t
	}
	return 1
}

// RequestID returns the request id.
func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(requestIDKey).(string)
	return s
}

// WriteJSON writes v with status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes the envelope. Unknown errors become 500 with a generic
// message (internals are logged, never leaked).
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var ae *APIError
	if !errors.As(err, &ae) {
		for _, m := range mappers {
			if ae = m(err); ae != nil {
				break
			}
		}
	}
	if ae == nil {
		slog.Error("internal error", "err", err, "path", r.URL.Path, "requestId", RequestID(r.Context()))
		ae = &APIError{Status: 500, Code: "INTERNAL_ERROR", Message: "an internal error occurred"}
	}
	if ae.Status == http.StatusTooManyRequests && w.Header().Get("Retry-After") == "" {
		w.Header().Set("Retry-After", "1")
	}
	WriteJSON(w, ae.Status, map[string]any{"error": map[string]any{
		"code": ae.Code, "message": ae.Message, "details": ae.Details, "requestId": RequestID(r.Context()),
	}})
}

// DecodeJSON decodes a size-limited JSON body. strict rejects unknown fields.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any, maxBytes int64, strict bool) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return TooLarge(fmt.Sprintf("body exceeds %d bytes", maxBytes))
		}
		if errors.Is(err, io.EOF) {
			return BadRequest("request body is required")
		}
		return BadRequest("invalid JSON: " + err.Error())
	}
	return nil
}

// ReadBody reads a size-limited raw body.
func ReadBody(w http.ResponseWriter, r *http.Request, maxBytes int64) ([]byte, error) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, TooLarge(fmt.Sprintf("body exceeds %d bytes", maxBytes))
		}
		return nil, BadRequest("failed to read body")
	}
	return b, nil
}

// ---- middleware

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusWriter) WriteHeader(c int) { s.status = c; s.ResponseWriter.WriteHeader(c) }
func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = 200
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Flush supports SSE through the wrapper.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Chain applies middleware outermost-first.
func Chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// Observe adds request IDs, panic recovery and structured access logs.
func Observe(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-Id")
			if id == "" || len(id) > 64 {
				id = newRequestID()
			}
			w.Header().Set("X-Request-Id", id)
			ctx := context.WithValue(r.Context(), requestIDKey, id)
			r = r.WithContext(ctx)
			sw := &statusWriter{ResponseWriter: w}
			start := time.Now()
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic", "err", rec, "stack", string(debug.Stack()), "requestId", id)
					if sw.status == 0 {
						WriteError(sw, r, fmt.Errorf("panic: %v", rec))
					}
				}
				lvl := slog.LevelDebug
				if sw.status >= 500 {
					lvl = slog.LevelError
				} else if strings.HasPrefix(r.URL.Path, "/api/") && r.Method != http.MethodGet {
					lvl = slog.LevelInfo
				}
				log.Log(ctx, lvl, "http", "method", r.Method, "path", r.URL.Path, "status", sw.status,
					"bytes", sw.bytes, "dur_ms", time.Since(start).Milliseconds(), "requestId", id)
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

// SecurityHeaders sets conservative defaults; framing is allowed only for the
// Abacus preview (frame-ancestors) instead of X-Frame-Options.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy", "frame-ancestors 'self' https://*.abacus.ai")
		next.ServeHTTP(w, r)
	})
}

// CORS allows configured origins (none by default: same-origin UI).
func CORS(origins []string) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range origins {
		allowed[o] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if o := r.Header.Get("Origin"); o != "" && (allowed[o] || allowed["*"]) {
				w.Header().Set("Access-Control-Allow-Origin", o)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				if r.Method == http.MethodOptions {
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
