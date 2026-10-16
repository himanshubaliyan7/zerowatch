package authn

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerowatch/zerowatch/internal/httpx"
)

// ---- fake identity provider

type signer struct {
	kid string
	alg string
	rsa *rsa.PrivateKey
	ec  *ecdsa.PrivateKey
}

func newRSASigner(t *testing.T, kid string) signer {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return signer{kid: kid, alg: algRS256, rsa: k}
}

func newECSigner(t *testing.T, kid string) signer {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return signer{kid: kid, alg: algES256, ec: k}
}

func (s signer) jwk() map[string]string {
	if s.rsa != nil {
		return map[string]string{"kty": "RSA", "kid": s.kid, "use": "sig", "alg": algRS256,
			"n": b64(s.rsa.N.Bytes()), "e": b64(big.NewInt(int64(s.rsa.E)).Bytes())}
	}
	return map[string]string{"kty": "EC", "kid": s.kid, "crv": "P-256",
		"x": b64(pad32(s.ec.X.Bytes())), "y": b64(pad32(s.ec.Y.Bytes()))}
}

func (s signer) sign(t *testing.T, header map[string]any, claims map[string]any) string {
	if header == nil {
		header = map[string]any{"alg": s.alg, "kid": s.kid, "typ": "JWT"}
	}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	input := b64(hb) + "." + b64(cb)
	d := sha256.Sum256([]byte(input))
	var sig []byte
	var err error
	if s.rsa != nil {
		sig, err = rsa.SignPKCS1v15(rand.Reader, s.rsa, crypto.SHA256, d[:])
	} else {
		var r, ss *big.Int
		r, ss, err = ecdsa.Sign(rand.Reader, s.ec, d[:])
		sig = append(pad32(r.Bytes()), pad32(ss.Bytes())...)
	}
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + b64(sig)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func pad32(b []byte) []byte {
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

type fakeIdP struct {
	t   *testing.T
	srv *httptest.Server
	now func() time.Time

	mu        sync.Mutex
	signers   []signer // published in JWKS; the first one signs
	claims    map[string]any
	userinfo  map[string]any
	nonce     string
	challenge string
	jwksHits  int
}

func newIdP(t *testing.T, now func() time.Time, s ...signer) *fakeIdP {
	idp := &fakeIdP{t: t, now: now, signers: s, claims: map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, 200, map[string]any{"issuer": idp.srv.URL, "authorization_endpoint": idp.srv.URL + "/authorize",
			"token_endpoint": idp.srv.URL + "/token", "userinfo_endpoint": idp.srv.URL + "/userinfo", "jwks_uri": idp.srv.URL + "/jwks",
			"token_endpoint_auth_methods_supported": []string{"client_secret_basic"}})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		idp.mu.Lock()
		defer idp.mu.Unlock()
		idp.jwksHits++
		var keys []map[string]string
		for _, s := range idp.signers {
			keys = append(keys, s.jwk())
		}
		httpx.WriteJSON(w, 200, map[string]any{"keys": keys})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		idp.mu.Lock()
		defer idp.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		id, secret, _ := r.BasicAuth()
		if r.PostForm.Get("code") != "good-code" || b64(sum[:]) != idp.challenge || id != "zw" || secret != "s3cret" {
			httpx.WriteJSON(w, 400, map[string]string{"error": "invalid_grant"})
			return
		}
		httpx.WriteJSON(w, 200, map[string]any{"id_token": idp.signers[0].sign(t, nil, idp.idClaims()), "access_token": "at-1", "token_type": "Bearer"})
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		idp.mu.Lock()
		defer idp.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer at-1" || idp.userinfo == nil {
			w.WriteHeader(401)
			return
		}
		httpx.WriteJSON(w, 200, idp.userinfo)
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

// idClaims: a valid token for the last authorize request, plus overrides.
func (idp *fakeIdP) idClaims() map[string]any {
	now := idp.now()
	c := map[string]any{"iss": idp.srv.URL, "aud": "zw", "sub": "user-1", "email": "ada@example.com",
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "nonce": idp.nonce}
	for k, v := range idp.claims {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func (idp *fakeIdP) set(f func(*fakeIdP)) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	f(idp)
}

// ---- harness

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type harness struct {
	t   *testing.T
	idp *fakeIdP
	a   *Authenticator
	h   http.Handler
	clk *clock
}

func newHarness(t *testing.T, mutate func(*Config), signers ...signer) *harness {
	clk := &clock{t: time.Now()}
	if len(signers) == 0 {
		signers = []signer{newRSASigner(t, "rsa-1")}
	}
	idp := newIdP(t, clk.now, signers...)
	cfg := Config{Issuer: idp.srv.URL, ClientID: "zw", ClientSecret: "s3cret", RedirectURL: "http://zw.test/auth/callback",
		RoleClaim: "roles", RoleMap: map[string]string{"zw-admins": "admin", "zw-editors": "editor"}, DefaultRole: "viewer",
		SessionKey: []byte(strings.Repeat("k", 32)), HTTPClient: idp.srv.Client(), Now: clk.now,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if mutate != nil {
		mutate(&cfg)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Register(mux, a)
	ok := func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, 200, map[string]any{"tenant": httpx.Tenant(r.Context())})
	}
	mux.HandleFunc("GET /api/v1/stats", ok)
	mux.HandleFunc("POST /api/v1/dashboards", ok)
	mux.HandleFunc("POST /api/v1/query", ok)
	mux.HandleFunc("POST /v1/metrics", ok)
	ks := httpx.NewKeyStore(nil)
	ks.AddKey("ingest-key-0123456789", 3, httpx.RoleEditor)
	h := httpx.Chain(mux, httpx.AuthWithSessions(ks, a, IsPublic), RBAC)
	return &harness{t: t, idp: idp, a: a, h: h, clk: clk}
}

func (hs *harness) do(method, path string, cookies []*http.Cookie, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://zw.test"+path, strings.NewReader("{}"))
	for _, c := range cookies {
		req.AddCookie(c)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	return rec
}

func cookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// login runs /auth/login and the provider leg; returns the callback response.
func (hs *harness) login(code string) *httptest.ResponseRecorder {
	hs.t.Helper()
	rec := hs.do("GET", "/auth/login?return=/%23/dashboards", nil, nil)
	if rec.Code != http.StatusFound {
		hs.t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	q := loc.Query()
	if !strings.HasPrefix(loc.String(), hs.idp.srv.URL+"/authorize?") || q.Get("code_challenge_method") != "S256" ||
		q.Get("client_id") != "zw" || q.Get("redirect_uri") != "http://zw.test/auth/callback" || q.Get("scope") != "openid profile email" {
		hs.t.Fatalf("authorize redirect %s", loc)
	}
	hs.idp.set(func(i *fakeIdP) { i.nonce, i.challenge = q.Get("nonce"), q.Get("code_challenge") })
	flowC := cookie(rec, flowCookie)
	if flowC == nil || !flowC.HttpOnly || flowC.SameSite != http.SameSiteLaxMode {
		hs.t.Fatalf("flow cookie %+v", flowC)
	}
	return hs.do("GET", "/auth/callback?code="+code+"&state="+url.QueryEscape(q.Get("state")), []*http.Cookie{flowC}, nil)
}

func (hs *harness) session() *http.Cookie {
	hs.t.Helper()
	rec := hs.login("good-code")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/#/dashboards" {
		hs.t.Fatalf("callback: %d %s %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	c := cookie(rec, SessionCookie)
	if c == nil || !c.HttpOnly || c.Path != "/" {
		hs.t.Fatalf("session cookie %+v", c)
	}
	return c
}

func (hs *harness) me(cookies ...*http.Cookie) Me {
	rec := hs.do("GET", "/api/v1/auth/me", cookies, nil)
	var m Me
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &m) != nil {
		hs.t.Fatalf("me %d %s", rec.Code, rec.Body)
	}
	return m
}

// ---- tests

func TestLoginFlowRS256(t *testing.T) {
	hs := newHarness(t, nil)
	if m := hs.me(); m.Authenticated || !m.OIDC || m.Method != "none" {
		t.Fatalf("anonymous me %+v", m)
	}
	if rec := hs.do("GET", "/api/v1/stats", nil, nil); rec.Code != 401 {
		t.Fatalf("anonymous stats %d", rec.Code)
	}
	s := hs.session()
	m := hs.me(s)
	if !m.Authenticated || m.Method != "oidc" || m.User != "ada@example.com" || m.Role != "viewer" || m.Tenant != 1 {
		t.Fatalf("me %+v", m)
	}
	if rec := hs.do("GET", "/api/v1/stats", []*http.Cookie{s}, nil); rec.Code != 200 {
		t.Fatalf("viewer GET %d", rec.Code)
	}
	if rec := hs.do("POST", "/api/v1/query", []*http.Cookie{s}, nil); rec.Code != 200 {
		t.Fatalf("viewer query %d", rec.Code)
	}
	if rec := hs.do("POST", "/api/v1/dashboards", []*http.Cookie{s}, nil); rec.Code != 403 {
		t.Fatalf("viewer write should be 403, got %d", rec.Code)
	}
	// Ingest never accepts the browser session; the API key works.
	if rec := hs.do("POST", "/v1/metrics", []*http.Cookie{s}, nil); rec.Code != 403 {
		t.Fatalf("session on ingest %d", rec.Code)
	}
	if rec := hs.do("POST", "/v1/metrics", nil, map[string]string{"X-API-Key": "ingest-key-0123456789"}); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"tenant":3`) {
		t.Fatalf("key on ingest %d %s", rec.Code, rec.Body)
	}
	// Session expiry.
	hs.clk.add(8*time.Hour + time.Second)
	if m := hs.me(s); m.Authenticated {
		t.Fatal("expired session still accepted")
	}
}

func TestLoginFlowES256RoleMapTenantAndRotation(t *testing.T) {
	hs := newHarness(t, func(c *Config) { c.TenantClaim = "zw.tenant" }, newECSigner(t, "ec-1"))
	hs.idp.set(func(i *fakeIdP) {
		i.claims["roles"] = []string{"staff", "zw-editors"}
		i.claims["zw"] = map[string]any{"tenant": "42"}
	})
	s := hs.session()
	if m := hs.me(s); m.Role != "editor" || m.Tenant != 42 {
		t.Fatalf("me %+v", m)
	}
	if rec := hs.do("POST", "/api/v1/dashboards", []*http.Cookie{s}, map[string]string{"Origin": "http://zw.test"}); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"tenant":42`) {
		t.Fatalf("editor write %d %s", rec.Code, rec.Body)
	}
	// CSRF: a cross-site write with a valid cookie is refused.
	if rec := hs.do("POST", "/api/v1/dashboards", []*http.Cookie{s}, map[string]string{"Sec-Fetch-Site": "cross-site"}); rec.Code != 403 {
		t.Fatalf("cross-site write %d", rec.Code)
	}
	if rec := hs.do("POST", "/api/v1/dashboards", []*http.Cookie{s}, map[string]string{"Origin": "https://evil.example"}); rec.Code != 403 {
		t.Fatalf("foreign origin write %d", rec.Code)
	}

	// Rotation: the IdP swaps to a new key; after the miss interval the
	// unknown kid triggers a refetch and sign-in keeps working.
	hs.idp.set(func(i *fakeIdP) { i.signers = []signer{newECSigner(t, "ec-2"), newRSASigner(t, "rsa-9")} })
	hs.clk.add(11 * time.Second)
	if rec := hs.login("good-code"); rec.Code != http.StatusFound {
		t.Fatalf("after rotation: %d %s", rec.Code, rec.Body)
	}
	// A forged kid within the miss interval doesn't refetch the JWKS.
	hits := hs.idp.jwksHits
	if _, err := hs.a.verifyIDToken(context.Background(), newRSASigner(t, "forged").sign(t, nil, map[string]any{}), "x"); err == nil || hs.idp.jwksHits != hits {
		t.Fatalf("forged kid: err=%v refetches=%d", err, hs.idp.jwksHits-hits)
	}
}

func TestUserinfoFallbackAndDottedClaim(t *testing.T) {
	hs := newHarness(t, func(c *Config) { c.RoleClaim = "realm_access.roles"; c.DefaultRole = "" })
	hs.idp.set(func(i *fakeIdP) {
		i.userinfo = map[string]any{"sub": "user-1", "realm_access": map[string]any{"roles": []string{"offline_access", "zw-admins"}}}
	})
	s := hs.session()
	if m := hs.me(s); m.Role != "admin" {
		t.Fatalf("me %+v", m)
	}
	// userinfo for another subject is ignored -> no role, no default -> denied.
	hs.idp.set(func(i *fakeIdP) { i.userinfo["sub"] = "someone-else" })
	if rec := hs.login("good-code"); rec.Code != 403 || cookie(rec, SessionCookie) != nil {
		t.Fatalf("mismatched userinfo should deny: %d", rec.Code)
	}
}

func TestDeniedWithoutRole(t *testing.T) {
	hs := newHarness(t, func(c *Config) { c.DefaultRole = "" })
	hs.idp.set(func(i *fakeIdP) { i.claims["roles"] = "staff contractors" })
	rec := hs.login("good-code")
	if rec.Code != 403 || cookie(rec, SessionCookie) != nil || !strings.Contains(rec.Body.String(), "Access denied") {
		t.Fatalf("want 403 page, got %d %s", rec.Code, rec.Body)
	}
}

func TestCallbackRejects(t *testing.T) {
	hs := newHarness(t, nil)
	if rec := hs.login("bad-code"); rec.Code != 401 {
		t.Fatalf("bad code %d", rec.Code)
	}
	// Wrong state.
	rec := hs.do("GET", "/auth/login", nil, nil)
	flowC := cookie(rec, flowCookie)
	if rec := hs.do("GET", "/auth/callback?code=good-code&state=forged", []*http.Cookie{flowC}, nil); rec.Code != 400 {
		t.Fatalf("forged state %d", rec.Code)
	}
	// No flow cookie (another browser).
	if rec := hs.do("GET", "/auth/callback?code=good-code&state=x", nil, nil); rec.Code != 400 {
		t.Fatalf("no flow cookie %d", rec.Code)
	}
	// Provider error.
	if rec := hs.do("GET", "/auth/callback?error=access_denied", nil, nil); rec.Code != 401 {
		t.Fatalf("provider error %d", rec.Code)
	}
	// Expired flow.
	rec = hs.do("GET", "/auth/login", nil, nil)
	flowC = cookie(rec, flowCookie)
	loc, _ := url.Parse(rec.Header().Get("Location"))
	hs.clk.add(flowTTL + time.Second)
	if rec := hs.do("GET", "/auth/callback?code=good-code&state="+loc.Query().Get("state"), []*http.Cookie{flowC}, nil); rec.Code != 400 {
		t.Fatalf("expired flow %d", rec.Code)
	}
}

func TestIDTokenValidation(t *testing.T) {
	rs := newRSASigner(t, "rsa-1")
	hs := newHarness(t, nil, rs)
	if err := hs.a.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := hs.clk.now()
	base := func() map[string]any {
		return map[string]any{"iss": hs.idp.srv.URL, "aud": "zw", "sub": "u", "exp": now.Add(time.Minute).Unix(), "iat": now.Unix(), "nonce": "n1"}
	}
	with := func(k string, v any) map[string]any {
		c := base()
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
		return c
	}
	good := rs.sign(t, nil, base())
	if _, err := hs.a.verifyIDToken(context.Background(), good, "n1"); err != nil {
		t.Fatalf("good token: %v", err)
	}
	parts := strings.Split(good, ".")
	tampered := parts[0] + "." + b64([]byte(`{"iss":"`+hs.idp.srv.URL+`","aud":"zw","sub":"admin","exp":9999999999,"nonce":"n1"}`)) + "." + parts[2]
	otherKey := newRSASigner(t, "rsa-1") // same kid, different key
	hmacTok := func() string {
		h := b64([]byte(`{"alg":"HS256","kid":"rsa-1"}`))
		p := b64([]byte(`{"iss":"x"}`))
		m := hmac.New(sha256.New, []byte("zw"))
		m.Write([]byte(h + "." + p))
		return h + "." + p + "." + b64(m.Sum(nil))
	}()
	cases := map[string]string{
		"tampered payload": tampered,
		"wrong key":        otherKey.sign(t, nil, base()),
		"expired":          rs.sign(t, nil, with("exp", now.Add(-2*time.Minute).Unix())),
		"no exp":           rs.sign(t, nil, with("exp", nil)),
		"not yet valid":    rs.sign(t, nil, with("nbf", now.Add(5*time.Minute).Unix())),
		"wrong aud":        rs.sign(t, nil, with("aud", "other-client")),
		"aud list no azp":  rs.sign(t, nil, with("aud", []string{"zw", "other"})),
		"wrong azp":        rs.sign(t, nil, with("azp", "other")),
		"wrong issuer":     rs.sign(t, nil, with("iss", "https://evil.example")),
		"wrong nonce":      rs.sign(t, nil, with("nonce", "n2")),
		"no sub":           rs.sign(t, nil, with("sub", nil)),
		"alg none":         b64([]byte(`{"alg":"none"}`)) + "." + parts[1] + ".",
		"alg HS256":        hmacTok,
		"unknown kid":      rs.sign(t, map[string]any{"alg": "RS256", "kid": "nope"}, base()),
		"garbage":          "not.a.jwt.at.all",
	}
	for name, tok := range cases {
		if _, err := hs.a.verifyIDToken(context.Background(), tok, "n1"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Within the clock skew is fine.
	if _, err := hs.a.verifyIDToken(context.Background(), rs.sign(t, nil, with("exp", now.Add(-30*time.Second).Unix())), "n1"); err != nil {
		t.Errorf("skew: %v", err)
	}
	// aud list with matching azp is fine.
	c := with("aud", []string{"zw", "account"})
	c["azp"] = "zw"
	if _, err := hs.a.verifyIDToken(context.Background(), rs.sign(t, nil, c), "n1"); err != nil {
		t.Errorf("aud list + azp: %v", err)
	}
}

func TestDiscoveryIssuerMismatch(t *testing.T) {
	hs := newHarness(t, nil)
	a, err := New(Config{Issuer: hs.idp.srv.URL + "/", ClientID: "zw", RedirectURL: "http://zw.test/auth/callback",
		SessionKey: []byte(strings.Repeat("k", 32)), HTTPClient: hs.idp.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Discover(context.Background()); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("want issuer mismatch, got %v", err)
	}
}

func TestSessionCookieTampering(t *testing.T) {
	hs := newHarness(t, nil)
	s := hs.session()
	b := []byte(s.Value)
	b[5] ^= 1
	if m := hs.me(&http.Cookie{Name: SessionCookie, Value: string(b)}); m.Authenticated {
		t.Fatal("tampered cookie accepted")
	}
	// A flow cookie can't stand in for a session (purpose binding).
	rec := hs.do("GET", "/auth/login", nil, nil)
	if m := hs.me(&http.Cookie{Name: SessionCookie, Value: cookie(rec, flowCookie).Value}); m.Authenticated {
		t.Fatal("flow cookie accepted as session")
	}
	// Another key can't open it.
	other, _ := newSealer([]byte(strings.Repeat("x", 32)))
	var v session
	if other.open("session", s.Value, &v) == nil {
		t.Fatal("opened with a different key")
	}
	// Logout clears it.
	out := hs.do("GET", "/auth/logout", []*http.Cookie{s}, nil)
	if c := cookie(out, SessionCookie); out.Code != 302 || c == nil || c.MaxAge >= 0 {
		t.Fatalf("logout %d %+v", out.Code, c)
	}
}

func TestStaleAPIKeyDoesNotHideSessionOnMe(t *testing.T) {
	hs := newHarness(t, nil)
	s := hs.session()
	m := hs.me(s) // baseline
	rec := hs.do("GET", "/api/v1/auth/me", []*http.Cookie{s}, map[string]string{"X-API-Key": "stale-key-0000000000"})
	var m2 Me
	_ = json.Unmarshal(rec.Body.Bytes(), &m2)
	if m2.Method != "oidc" || m2.User != m.User {
		t.Fatalf("me with stale key %+v", m2)
	}
	// But on a protected route the explicit (bad) key is an error.
	if rec := hs.do("GET", "/api/v1/stats", []*http.Cookie{s}, map[string]string{"X-API-Key": "stale-key-0000000000"}); rec.Code != 401 {
		t.Fatalf("bad key with cookie %d", rec.Code)
	}
}

func TestSafeReturnAndRoleMap(t *testing.T) {
	for in, want := range map[string]string{"/#/x": "/#/x", "": "/", "//evil.com": "/", "/\\evil.com": "/", "https://evil.com": "/", "/a\r\nb": "/"} {
		if got := safeReturn(in); got != want {
			t.Errorf("safeReturn(%q) = %q", in, got)
		}
	}
	m, err := ParseRoleMap("zw-admins=admin, team/ops = editor")
	if err != nil || m["zw-admins"] != "admin" || m["team/ops"] != "editor" {
		t.Fatalf("%v %v", m, err)
	}
	if _, err := ParseRoleMap("x=root"); err == nil {
		t.Fatal("bad role accepted")
	}
	if _, err := New(Config{Issuer: "https://idp", ClientID: "c", RedirectURL: "https://zw/auth/callback", SessionKey: []byte("short")}); err == nil {
		t.Fatal("short session key accepted")
	}
}
