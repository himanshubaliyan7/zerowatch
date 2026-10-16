package httpx

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Role is a caller's permission level; higher includes lower.
type Role uint8

// Roles, lowest first. RoleNone is "not authenticated".
const (
	RoleNone Role = iota
	RoleViewer
	RoleEditor
	RoleAdmin
)

func (r Role) String() string {
	switch r {
	case RoleViewer:
		return "viewer"
	case RoleEditor:
		return "editor"
	case RoleAdmin:
		return "admin"
	}
	return ""
}

// ParseRole parses viewer|editor|admin (case-insensitive).
func ParseRole(s string) (Role, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "viewer":
		return RoleViewer, true
	case "editor":
		return RoleEditor, true
	case "admin":
		return RoleAdmin, true
	}
	return RoleNone, false
}

// Authentication methods recorded on a Principal.
const (
	MethodNone   = "none" // auth disabled (development)
	MethodAPIKey = "apikey"
	MethodSigV4  = "sigv4"
	MethodOIDC   = "oidc"
)

// Principal is the authenticated caller.
type Principal struct {
	Subject string // OIDC sub, or "key:<digest prefix>" / "aws:<access key id>"
	Email   string
	Role    Role
	Tenant  uint32
	Method  string
}

// WithPrincipal stores the caller (and its tenant).
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(WithTenant(ctx, p.Tenant), principalKey, p)
}

// PrincipalFrom returns the caller, if the request was authenticated.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey).(Principal)
	return p, ok
}

type principalCtxKey struct{}

var principalKey principalCtxKey

// Forbidden is the 403 envelope (authenticated, but not allowed).
func Forbidden(msg string) *APIError {
	return &APIError{Status: http.StatusForbidden, Code: "FORBIDDEN", Message: msg}
}

// Sessions authenticates browser sessions (internal/authn's OIDC cookie).
// Kept as an interface so httpx stays free of the OIDC code.
type Sessions interface {
	Session(r *http.Request) (Principal, bool)
}

// KeyStore authenticates API keys. Keys are held as SHA-256 digests and
// compared in constant time, so a heap dump or timing probe can't recover them.
type KeyStore struct {
	entries []keyEntry
	aws     map[string]AWSCredential // access key id -> credential; nil = SigV4 not verified
}

type keyEntry struct {
	digest [32]byte
	tenant uint32
	role   Role
}

// NewKeyStore builds a store from key -> tenant pairs; every key is admin
// (the behaviour before roles existed). Use AddKey for other roles.
func NewKeyStore(keys map[string]uint32) *KeyStore {
	ks := &KeyStore{}
	for k, t := range keys {
		ks.AddKey(k, t, RoleAdmin)
	}
	return ks
}

// AddKey adds a key with a tenant and role (RoleNone means admin).
func (k *KeyStore) AddKey(key string, tenant uint32, role Role) {
	if role == RoleNone {
		role = RoleAdmin
	}
	k.entries = append(k.entries, keyEntry{sha256.Sum256([]byte(key)), tenant, role})
}

// Enabled reports whether any key is configured.
func (k *KeyStore) Enabled() bool { return k != nil && (len(k.entries) > 0 || len(k.aws) > 0) }

// Lookup returns the tenant for a key.
func (k *KeyStore) Lookup(key string) (uint32, bool) {
	t, _, ok := k.LookupRole(key)
	return t, ok
}

// LookupRole returns the tenant and role for a key.
func (k *KeyStore) LookupRole(key string) (uint32, Role, bool) {
	if k == nil {
		return 0, RoleNone, false
	}
	d := sha256.Sum256([]byte(key))
	var tenant uint32
	role := RoleNone
	found := 0
	for _, e := range k.entries { // no early exit: constant time in #keys
		if subtle.ConstantTimeCompare(d[:], e.digest[:]) == 1 {
			tenant, role, found = e.tenant, e.role, 1
		}
	}
	return tenant, role, found == 1
}

// ExtractKey finds a credential in (order): Authorization: Bearer, X-API-Key,
// AWS SigV4 Credential access-key-id (CloudWatch SDK compatibility; only used
// when no AWS credentials are configured, in which case the signature is NOT
// verified), and ?api_key= (EventSource cannot set headers).
func ExtractKey(r *http.Request) string {
	if a := r.Header.Get("Authorization"); a != "" {
		if strings.HasPrefix(a, "Bearer ") {
			return strings.TrimSpace(a[7:])
		}
		if strings.HasPrefix(a, "AWS4-HMAC-SHA256") {
			if i := strings.Index(a, "Credential="); i >= 0 {
				cred := a[i+len("Credential="):]
				if j := strings.IndexByte(cred, '/'); j > 0 {
					return cred[:j]
				}
			}
		}
	}
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	return r.URL.Query().Get("api_key")
}

// Auth enforces API keys when configured; otherwise every request is tenant 1
// (development mode, logged loudly at startup by the caller). Paths in public
// are always allowed (health checks, SPA assets).
func Auth(ks *KeyStore, public func(*http.Request) bool) func(http.Handler) http.Handler {
	return AuthWithSessions(ks, nil, public)
}

// AuthWithSessions is Auth plus browser sessions. Credentials are tried in
// order: SigV4 (when AWS credentials are configured), an API key, then the
// session cookie. An explicit credential that fails is an error even when a
// cookie is present, except on public paths, where authentication is only
// best effort (so /api/v1/auth/me can say who is signed in). Auth is enforced
// when any key, AWS credential or a Sessions is configured; with none, every
// request is an admin of tenant 1. Which roles and methods a route accepts is
// decided after this, by the RBAC middleware (internal/authn).
func AuthWithSessions(ks *KeyStore, sess Sessions, public func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !ks.Enabled() && sess == nil {
				ctx := WithPrincipal(r.Context(), Principal{Role: RoleAdmin, Tenant: 1, Method: MethodNone})
				ctx = context.WithValue(ctx, clientKey, "ip:"+clientIP(r))
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			isPublic := public != nil && public(r)
			p, client, fail := authenticate(r, ks, sess, isPublic)
			if isPublic {
				if client != "" {
					r = r.WithContext(context.WithValue(WithPrincipal(r.Context(), p), clientKey, client))
				}
				next.ServeHTTP(w, r)
				return
			}
			if fail != nil {
				fail(w, r)
				return
			}
			ctx := WithPrincipal(r.Context(), p)
			ctx = context.WithValue(ctx, clientKey, client)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// authenticate returns the principal and its rate-limit key, or a writer for
// the error. client == "" means nobody was authenticated.
func authenticate(r *http.Request, ks *KeyStore, sess Sessions, public bool) (Principal, string, func(http.ResponseWriter, *http.Request)) {
	if ks != nil && ks.aws != nil && isSigV4(r) {
		cred, e := verifySigV4(r, ks.aws, time.Now())
		if e == nil {
			sum := sha256.Sum256([]byte(cred.AccessKeyID))
			return Principal{Subject: "aws:" + cred.AccessKeyID, Role: RoleAdmin, Tenant: cred.Tenant, Method: MethodSigV4},
				"aws:" + string(sum[:8]), nil
		}
		if !public {
			return Principal{}, "", func(w http.ResponseWriter, r *http.Request) { writeSigV4Error(w, r, e) }
		}
	} else if key := ExtractKey(r); key != "" {
		if tenant, role, ok := ks.LookupRole(key); ok {
			sum := sha256.Sum256([]byte(key))
			return Principal{Subject: "key:" + hexPrefix(sum[:4]), Role: role, Tenant: tenant, Method: MethodAPIKey},
				"key:" + string(sum[:8]), nil
		}
		if !public {
			return Principal{}, "", writeErr(Unauthorized("invalid API key"))
		}
	}
	if sess != nil {
		if p, ok := sess.Session(r); ok {
			if !safeMethod(r.Method) && !sameOrigin(r) {
				if public {
					return Principal{}, "", nil
				}
				return Principal{}, "", writeErr(Forbidden("cross-site request refused"))
			}
			p.Method = MethodOIDC
			sum := sha256.Sum256([]byte(p.Subject))
			return p, "oidc:" + string(sum[:8]), nil
		}
	}
	msg := "missing API key (Authorization: Bearer <key> or X-API-Key)"
	if sess != nil {
		msg = "sign in required (/auth/login) or an API key (Authorization: Bearer <key> or X-API-Key)"
	}
	return Principal{}, "", writeErr(Unauthorized(msg))
}

func writeErr(e *APIError) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) { WriteError(w, r, e) }
}

func hexPrefix(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// sameOrigin is the CSRF guard for cookie-authenticated writes (on top of
// SameSite=Lax): Fetch Metadata when the browser sends it, else Origin
// against the host the browser saw (proxies may rewrite Host, so the
// forwarded host headers count too). No Origin at all is a non-browser
// client or an old browser; SameSite already covers those.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "":
	default:
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	for _, h := range []string{r.Host, r.Header.Get("X-Forwarded-Host"), r.Header.Get("X-Original-Host")} {
		if h != "" && strings.EqualFold(u.Host, h) {
			return true
		}
	}
	return false
}

func clientIP(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		ip, _, _ := strings.Cut(xf, ",")
		return strings.TrimSpace(ip)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---- rate limiting

// RateLimiter is a per-client token bucket (keyed by API key digest or IP).
type RateLimiter struct {
	rate  float64
	burst float64
	mu    sync.Mutex
	b     map[string]*bucket
	now   func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter creates a limiter and starts idle-bucket cleanup.
func NewRateLimiter(rps float64, burst int) *RateLimiter {
	rl := &RateLimiter{rate: rps, burst: float64(burst), b: map[string]*bucket{}, now: time.Now}
	return rl
}

// Allow consumes one token; returns remaining tokens and seconds until next token.
func (rl *RateLimiter) Allow(key string) (bool, float64, float64) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := rl.now()
	b := rl.b[key]
	if b == nil {
		b = &bucket{tokens: rl.burst, last: now}
		rl.b[key] = b
		if len(rl.b) > 100_000 { // bound memory under key/IP churn
			rl.sweepLocked(now)
		}
	}
	b.tokens = math.Min(rl.burst, b.tokens+now.Sub(b.last).Seconds()*rl.rate)
	b.last = now
	if b.tokens < 1 {
		return false, b.tokens, (1 - b.tokens) / rl.rate
	}
	b.tokens--
	return true, b.tokens, 0
}

func (rl *RateLimiter) sweepLocked(now time.Time) {
	for k, b := range rl.b {
		if now.Sub(b.last) > time.Minute {
			delete(rl.b, k)
		}
	}
}

// Middleware applies the limiter after Auth (so clients are keyed by key).
func (rl *RateLimiter) Middleware(exempt func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if exempt != nil && exempt(r) {
				next.ServeHTTP(w, r)
				return
			}
			key, _ := r.Context().Value(clientKey).(string)
			if key == "" {
				key = "ip:" + clientIP(r)
			}
			ok, remaining, retry := rl.Allow(key)
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(int(rl.burst)))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(int(remaining)))
			if !ok {
				w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry))))
				WriteError(w, r, &APIError{Status: http.StatusTooManyRequests, Code: "RATE_LIMITED", Message: "too many requests"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// LogAuthMode prints the auth mode at startup.
func LogAuthMode(log *slog.Logger, ks *KeyStore) {
	if ks.Enabled() {
		log.Info("API key auth enabled", "keys", len(ks.entries), "awsCredentials", len(ks.aws))
		if len(ks.aws) == 0 {
			log.Warn("AWS SigV4 signatures are NOT verified (no --aws-credentials / ZW_AWS_CREDENTIALS); the access key id is mapped through --api-keys")
		}
	} else {
		log.Warn("API key auth DISABLED (no --api-keys / --aws-credentials); all requests map to tenant 1 — development only")
	}
}
