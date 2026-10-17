package authn

import (
	"net/http"
	"strings"

	"github.com/himanshubaliyan7/zerowatch/internal/httpx"
)

// Rule is one row of the authorization table. Method "*" matches any method.
// In Path, "*" matches one segment and a trailing "**" matches the rest
// (zero or more segments).
type Rule struct {
	Method  string
	Path    string
	Min     httpx.Role // RoleNone = public
	KeyOnly bool       // API keys / SigV4 only: never a browser session
}

const (
	viewer = httpx.RoleViewer
	editor = httpx.RoleEditor
	admin  = httpx.RoleAdmin
)

// Rules is the route -> minimum role table, first match wins. Anything under
// /api/, /v1/, /prometheus/ or /cloudwatch that no earlier row names needs
// admin, so a new endpoint is admin-only until someone lists it here.
var Rules = []Rule{
	// Who am I: public, answers "nobody" too.
	{"GET", "/api/v1/auth/me", httpx.RoleNone, false},

	// Ingest: machine credentials only, editor or above.
	{"POST", "/v1/metrics", editor, true},
	{"POST", "/v1/logs", editor, true},
	{"POST", "/v1/traces", editor, true},
	{"POST", "/api/v1/write", editor, true},
	{"POST", "/api/v1/ingest/**", editor, true},
	{"*", "/cloudwatch", editor, true},
	{"*", "/cloudwatch/**", editor, true},

	// Reads that need a body: viewer.
	{"POST", "/api/v1/query", viewer, false},
	{"POST", "/api/v1/forecast", viewer, false},
	{"POST", "/api/v1/anomaly", viewer, false},
	{"POST", "/api/v1/alarms/simulate", viewer, false},
	{"POST", "/api/v1/alarms/backtest", viewer, false},
	{"POST", "/api/v1/logs/search", viewer, false},

	// Content writes: editor.
	{"POST", "/api/v1/dashboards", editor, false},
	{"PUT", "/api/v1/dashboards/*", editor, false},
	{"DELETE", "/api/v1/dashboards/*", editor, false},
	{"POST", "/api/v1/alarms", editor, false},
	{"PUT", "/api/v1/alarms/*", editor, false},
	{"DELETE", "/api/v1/alarms/*", editor, false},
	{"POST", "/api/v1/logs/saved", editor, false},
	{"DELETE", "/api/v1/logs/saved/*", editor, false},
	{"POST", "/api/v1/reports/send", editor, false},

	// Prometheus-compatible query API: Grafana sends GET or form POSTs.
	{"GET", "/prometheus/**", viewer, false},
	{"HEAD", "/prometheus/**", viewer, false},
	{"POST", "/prometheus/**", viewer, false},
	{"*", "/prometheus/**", admin, false},

	// Every other read: viewer (includes supervisor and cluster status).
	{"GET", "/api/**", viewer, false},
	{"HEAD", "/api/**", viewer, false},

	// Everything else on the API (supervisor/cluster settings, new routes): admin.
	{"*", "/api/**", admin, false},
	{"*", "/v1/**", admin, true},
}

// Match returns the rule for a request; ok is false for paths outside the
// API (SPA, /auth/*, health checks), which are public.
func Match(method, path string) (Rule, bool) {
	for _, r := range Rules {
		if (r.Method == "*" || r.Method == method) && matchPath(r.Path, path) {
			return r, true
		}
	}
	return Rule{}, false
}

func matchPath(pattern, path string) bool {
	ps := strings.Split(strings.Trim(pattern, "/"), "/")
	xs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, p := range ps {
		if p == "**" && i == len(ps)-1 {
			return true
		}
		if i >= len(xs) || p != "*" && p != xs[i] || p == "*" && xs[i] == "" {
			return false
		}
	}
	return len(xs) == len(ps)
}

// RBAC enforces Rules on the principal set by httpx.AuthWithSessions. Place
// it right after the auth middleware.
func RBAC(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rule, ok := Match(r.Method, r.URL.Path)
		if !ok || rule.Min == httpx.RoleNone && !rule.KeyOnly {
			next.ServeHTTP(w, r)
			return
		}
		p, ok := httpx.PrincipalFrom(r.Context())
		if !ok {
			httpx.WriteError(w, r, httpx.Unauthorized("authentication required"))
			return
		}
		if rule.KeyOnly && p.Method == httpx.MethodOIDC {
			httpx.WriteError(w, r, httpx.Forbidden("this endpoint accepts API keys or SigV4 only, not a browser session"))
			return
		}
		if p.Role < rule.Min {
			httpx.WriteError(w, r, httpx.Forbidden("requires the "+rule.Min.String()+" role (you are "+p.Role.String()+")"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// IsPublic marks the auth routes the auth middleware must let through
// unauthenticated; combine it with api.IsPublic.
func IsPublic(r *http.Request) bool {
	return r.URL.Path == "/api/v1/auth/me" || strings.HasPrefix(r.URL.Path, "/auth/")
}

// Me is the response of GET /api/v1/auth/me.
type Me struct {
	Authenticated bool   `json:"authenticated"`
	User          string `json:"user"` // email, else sub; "" when nobody
	Subject       string `json:"sub,omitempty"`
	Role          string `json:"role"`
	Tenant        uint32 `json:"tenant"`
	Method        string `json:"method"` // oidc | apikey | sigv4 | none
	OIDC          bool   `json:"oidc"`   // sign-in with /auth/login is available
}

// MeHandler answers who the caller is. a may be nil (OIDC off).
func MeHandler(a *Authenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		me := Me{Method: httpx.MethodNone, OIDC: a != nil}
		if p, ok := httpx.PrincipalFrom(r.Context()); ok {
			me.Authenticated, me.Subject, me.Role, me.Tenant, me.Method = true, p.Subject, p.Role.String(), p.Tenant, p.Method
			me.User = p.Email
			if me.User == "" && p.Method != httpx.MethodNone {
				me.User = p.Subject
			}
		}
		httpx.WriteJSON(w, http.StatusOK, me)
	}
}

// Register adds GET /api/v1/auth/me and, when a is set, the /auth/* routes.
func Register(mux *http.ServeMux, a *Authenticator) {
	mux.HandleFunc("GET /api/v1/auth/me", MeHandler(a))
	if a == nil {
		return
	}
	mux.HandleFunc("GET /auth/login", a.Login)
	mux.HandleFunc("GET /auth/callback", a.Callback)
	mux.HandleFunc("GET /auth/logout", a.Logout)
	mux.HandleFunc("POST /auth/logout", a.Logout)
}
