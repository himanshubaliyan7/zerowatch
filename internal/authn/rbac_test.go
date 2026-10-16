package authn

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerowatch/zerowatch/internal/httpx"
)

func TestRBACTable(t *testing.T) {
	none := httpx.RoleNone
	cases := []struct {
		method, path string
		min          httpx.Role
		keyOnly      bool
		public       bool // no rule (outside the API)
	}{
		{"GET", "/", none, false, true},
		{"GET", "/assets/app.js", none, false, true},
		{"GET", "/auth/login", none, false, true},
		{"GET", "/healthz", none, false, true},
		{"GET", "/api/v1/auth/me", none, false, false},
		{"GET", "/api/v1/stats", viewer, false, false},
		{"GET", "/api/v1/supervisor", viewer, false, false},
		{"GET", "/api/v1/cluster", viewer, false, false},
		{"GET", "/api/v1/logs/tail", viewer, false, false},
		{"GET", "/api/v1/dashboards/abc", viewer, false, false},
		{"GET", "/api/v1/reports/summary.html", viewer, false, false},
		{"POST", "/api/v1/query", viewer, false, false},
		{"POST", "/api/v1/forecast", viewer, false, false},
		{"POST", "/api/v1/anomaly", viewer, false, false},
		{"POST", "/api/v1/alarms/simulate", viewer, false, false},
		{"POST", "/api/v1/alarms/backtest", viewer, false, false},
		{"POST", "/api/v1/logs/search", viewer, false, false},
		{"POST", "/api/v1/dashboards", editor, false, false},
		{"PUT", "/api/v1/dashboards/abc", editor, false, false},
		{"DELETE", "/api/v1/dashboards/abc", editor, false, false},
		{"POST", "/api/v1/alarms", editor, false, false},
		{"PUT", "/api/v1/alarms/a1", editor, false, false},
		{"DELETE", "/api/v1/alarms/a1", editor, false, false},
		{"POST", "/api/v1/logs/saved", editor, false, false},
		{"DELETE", "/api/v1/logs/saved/q1", editor, false, false},
		{"POST", "/api/v1/reports/send", editor, false, false},
		{"POST", "/v1/metrics", editor, true, false},
		{"POST", "/v1/logs", editor, true, false},
		{"POST", "/v1/traces", editor, true, false},
		{"POST", "/api/v1/write", editor, true, false},
		{"POST", "/api/v1/ingest/metrics", editor, true, false},
		{"POST", "/api/v1/ingest/logs", editor, true, false},
		{"POST", "/cloudwatch", editor, true, false},
		{"POST", "/cloudwatch/", editor, true, false},
		{"GET", "/prometheus/api/v1/query", viewer, false, false},
		{"POST", "/prometheus/api/v1/query_range", viewer, false, false},
		{"GET", "/prometheus/api/v1/label/__name__/values", viewer, false, false},
		{"DELETE", "/prometheus/api/v1/admin/tsdb", admin, false, false},
		// Not listed: admin (supervisor/cluster settings, future routes).
		{"POST", "/api/v1/supervisor/settings", admin, false, false},
		{"PUT", "/api/v1/cluster/config", admin, false, false},
		{"DELETE", "/api/v1/dashboards", admin, false, false},
		{"POST", "/api/v1/dashboards/abc/extra", admin, false, false},
		{"POST", "/v1/unknown", admin, true, false},
	}
	for _, c := range cases {
		r, ok := Match(c.method, c.path)
		if ok == c.public || ok && (r.Min != c.min || r.KeyOnly != c.keyOnly) {
			t.Errorf("%s %s: got %+v ok=%v, want min=%v keyOnly=%v public=%v", c.method, c.path, r, ok, c.min, c.keyOnly, c.public)
		}
	}
}

func TestRBACMiddleware(t *testing.T) {
	h := RBAC(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	run := func(method, path string, p *httpx.Principal) int {
		req := httptest.NewRequest(method, path, nil)
		if p != nil {
			req = req.WithContext(httpx.WithPrincipal(req.Context(), *p))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	v := &httpx.Principal{Role: viewer, Tenant: 1, Method: httpx.MethodOIDC}
	e := &httpx.Principal{Role: editor, Tenant: 1, Method: httpx.MethodOIDC}
	ad := &httpx.Principal{Role: admin, Tenant: 1, Method: httpx.MethodOIDC}
	keyViewer := &httpx.Principal{Role: viewer, Tenant: 1, Method: httpx.MethodAPIKey}
	keyEditor := &httpx.Principal{Role: editor, Tenant: 1, Method: httpx.MethodAPIKey}
	dev := &httpx.Principal{Role: admin, Tenant: 1, Method: httpx.MethodNone}
	for _, c := range []struct {
		method, path string
		p            *httpx.Principal
		want         int
	}{
		{"GET", "/", nil, 204},
		{"GET", "/api/v1/auth/me", nil, 204},
		{"GET", "/api/v1/stats", nil, 401},
		{"GET", "/api/v1/stats", v, 204},
		{"POST", "/api/v1/query", v, 204},
		{"POST", "/api/v1/dashboards", v, 403},
		{"POST", "/api/v1/dashboards", e, 204},
		{"POST", "/api/v1/supervisor/settings", e, 403},
		{"POST", "/api/v1/supervisor/settings", ad, 204},
		{"GET", "/prometheus/api/v1/query", nil, 401},
		{"POST", "/prometheus/api/v1/query", v, 204},
		{"POST", "/prometheus/api/v1/query_range", keyViewer, 204},
		{"POST", "/v1/metrics", ad, 403}, // browser session, even admin
		{"POST", "/v1/metrics", keyViewer, 403},
		{"POST", "/v1/metrics", keyEditor, 204},
		{"POST", "/cloudwatch", dev, 204}, // auth disabled: everything allowed
		{"DELETE", "/api/v1/alarms/x", dev, 204},
	} {
		if got := run(c.method, c.path, c.p); got != c.want {
			t.Errorf("%s %s as %+v: %d, want %d", c.method, c.path, c.p, got, c.want)
		}
	}
}
