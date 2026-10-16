package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeSessions struct{ p Principal }

func (f fakeSessions) Session(r *http.Request) (Principal, bool) {
	if c, err := r.Cookie("s"); err == nil && c.Value == "ok" {
		return f.p, true
	}
	return Principal{}, false
}

func TestKeyRolesAndSessions(t *testing.T) {
	ks := NewKeyStore(map[string]uint32{"legacy-key-0123456789": 2})
	ks.AddKey("viewer-key-0123456789", 5, RoleViewer)
	if _, r, ok := ks.LookupRole("legacy-key-0123456789"); !ok || r != RoleAdmin {
		t.Fatalf("legacy keys must stay admin, got %v", r)
	}
	var got Principal
	h := AuthWithSessions(ks, fakeSessions{Principal{Subject: "u1", Email: "u@x", Role: RoleEditor, Tenant: 9}}, func(r *http.Request) bool {
		return r.URL.Path == "/me"
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = PrincipalFrom(r.Context())
		w.WriteHeader(204)
	}))
	do := func(method, path, key string, cookie bool, hdr ...string) int {
		got = Principal{}
		req := httptest.NewRequest(method, path, nil)
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		if cookie {
			req.AddCookie(&http.Cookie{Name: "s", Value: "ok"})
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := do("GET", "/api/x", "viewer-key-0123456789", false); c != 204 || got.Role != RoleViewer || got.Tenant != 5 || got.Method != MethodAPIKey {
		t.Fatalf("viewer key %d %+v", c, got)
	}
	if c := do("GET", "/api/x", "", true); c != 204 || got.Role != RoleEditor || got.Tenant != 9 || got.Method != MethodOIDC {
		t.Fatalf("session %d %+v", c, got)
	}
	if c := do("GET", "/api/x", "", false); c != 401 {
		t.Fatalf("nothing %d", c)
	}
	if c := do("GET", "/api/x", "wrong-key-00000000000", true); c != 401 {
		t.Fatalf("an explicit bad key must not fall back to the cookie: %d", c)
	}
	if c := do("GET", "/me", "wrong-key-00000000000", true); c != 204 || got.Method != MethodOIDC {
		t.Fatalf("public path falls back to the cookie: %d %+v", c, got)
	}
	if c := do("GET", "/me", "", false); c != 204 || got.Method != "" {
		t.Fatalf("public anonymous %d %+v", c, got)
	}
	// CSRF guard on cookie writes.
	if c := do("POST", "/api/x", "", true, "Sec-Fetch-Site", "cross-site"); c != 403 {
		t.Fatalf("cross-site %d", c)
	}
	if c := do("POST", "/api/x", "", true, "Sec-Fetch-Site", "same-origin"); c != 204 {
		t.Fatalf("same-origin %d", c)
	}
	if c := do("POST", "/api/x", "", true, "Origin", "http://example.com"); c != 204 { // httptest Host is example.com
		t.Fatalf("matching origin %d", c)
	}
	if c := do("POST", "/api/x", "", true, "Origin", "https://evil.test"); c != 403 {
		t.Fatalf("foreign origin %d", c)
	}
	// Keys don't need the CSRF guard (not ambient credentials).
	if c := do("POST", "/api/x", "viewer-key-0123456789", false, "Sec-Fetch-Site", "cross-site"); c != 204 {
		t.Fatalf("key cross-site %d", c)
	}
}

func TestAuthDisabledIsDevAdmin(t *testing.T) {
	var got Principal
	h := Auth(NewKeyStore(nil), nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = PrincipalFrom(r.Context())
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/x", nil))
	if got.Role != RoleAdmin || got.Tenant != 1 || got.Method != MethodNone {
		t.Fatalf("%+v", got)
	}
}
