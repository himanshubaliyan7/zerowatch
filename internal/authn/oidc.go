// Package authn signs browser users in with any OpenID Connect provider
// (Keycloak, Auth0, Okta, Google, Entra ID, ...) and authorizes every API
// request against a route -> minimum-role table (rbac.go).
//
// The flow is authorization code + PKCE (S256) with state and nonce. ID
// tokens are verified against the provider's JWKS (RS256, ES256; keys cached
// and refetched on rotation). The session is a stateless AES-GCM encrypted,
// HMAC-signed cookie holding sub, email, role, tenant and expiry, so any node
// sharing the session key accepts it and there is no server state. Ingest
// endpoints never accept the cookie (API keys or SigV4 only). Only the
// standard library is used.
package authn

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/httpx"
)

// Config configures the OIDC client. Issuer empty means OIDC is off.
type Config struct {
	Issuer       string   // e.g. https://kc.example.com/realms/acme (exactly the token's iss)
	ClientID     string   //
	ClientSecret string   // optional: public clients rely on PKCE alone
	RedirectURL  string   // https://zerowatch.example.com/auth/callback, registered at the IdP
	Scopes       []string // default openid, profile, email

	// RoleClaim is a dotted path into the ID token (then userinfo) claims,
	// e.g. "roles", "groups", "realm_access.roles". Default "roles". The value
	// may be a string, a space-separated string or a list.
	RoleClaim string
	// RoleMap maps claim values to viewer|editor|admin. A claim value that is
	// itself a role name counts even without a map entry. The highest wins.
	RoleMap map[string]string
	// DefaultRole applies when no claim value maps; empty denies sign-in.
	DefaultRole string
	// TenantClaim is an optional dotted path holding a numeric tenant id;
	// DefaultTenant (0 = 1) applies when it is empty or the claim is absent.
	TenantClaim   string
	DefaultTenant uint32

	// SessionKey (>= 32 bytes) seals the cookies. Empty = random per process:
	// sessions then die on restart and don't work across nodes.
	SessionKey   []byte
	CookieSecure bool          // Secure cookies; set when served over https
	SessionTTL   time.Duration // default 8h

	HTTPClient *http.Client     // default: 10 s timeout
	Log        *slog.Logger     //
	Now        func() time.Time // tests
}

// Cookie names. The flow cookie lives only for the login round trip.
const (
	SessionCookie = "zw_session"
	flowCookie    = "zw_oidc_flow"
	clockSkew     = time.Minute
	flowTTL       = 10 * time.Minute
)

// Authenticator runs the OIDC flow and implements httpx.Sessions.
type Authenticator struct {
	cfg     Config
	roleMap map[string]httpx.Role
	defRole httpx.Role
	client  *http.Client
	seal    *sealer
	now     func() time.Time
	log     *slog.Logger

	mu      sync.Mutex
	meta    *providerMeta
	metaErr time.Time // last failed discovery (backoff)
	jwks    *keySet
}

type providerMeta struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	UserinfoEndpoint      string   `json:"userinfo_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	TokenAuthMethods      []string `json:"token_endpoint_auth_methods_supported"`
}

// New validates cfg. Discovery is lazy (first login), so a provider that is
// down at startup doesn't stop the node; call Discover to check it early.
func New(cfg Config) (*Authenticator, error) {
	if cfg.Issuer == "" {
		return nil, errors.New("oidc: issuer is required")
	}
	if cfg.ClientID == "" {
		return nil, errors.New("oidc: client id is required")
	}
	ru, err := url.Parse(cfg.RedirectURL)
	if err != nil || (ru.Scheme != "https" && ru.Scheme != "http") || ru.Host == "" {
		return nil, errors.New("oidc: redirect url must be an absolute http(s) URL ending in /auth/callback")
	}
	if !strings.HasPrefix(cfg.Issuer, "https://") && !strings.HasPrefix(cfg.Issuer, "http://") {
		return nil, errors.New("oidc: issuer must be an http(s) URL")
	}
	a := &Authenticator{cfg: cfg, roleMap: map[string]httpx.Role{}, client: cfg.HTTPClient, now: cfg.Now, log: cfg.Log}
	if a.client == nil {
		a.client = &http.Client{Timeout: 10 * time.Second}
	}
	if a.now == nil {
		a.now = time.Now
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	if len(a.cfg.Scopes) == 0 {
		a.cfg.Scopes = []string{"openid", "profile", "email"}
	} else if !contains(a.cfg.Scopes, "openid") {
		a.cfg.Scopes = append([]string{"openid"}, a.cfg.Scopes...)
	}
	if a.cfg.RoleClaim == "" {
		a.cfg.RoleClaim = "roles"
	}
	if a.cfg.SessionTTL <= 0 {
		a.cfg.SessionTTL = 8 * time.Hour
	}
	if a.cfg.DefaultTenant == 0 {
		a.cfg.DefaultTenant = 1
	}
	for k, v := range cfg.RoleMap {
		r, ok := httpx.ParseRole(v)
		if !ok {
			return nil, fmt.Errorf("oidc: role map entry %q: role must be viewer, editor or admin", k)
		}
		a.roleMap[k] = r
	}
	if cfg.DefaultRole != "" {
		r, ok := httpx.ParseRole(cfg.DefaultRole)
		if !ok {
			return nil, errors.New("oidc: default role must be viewer, editor, admin or empty")
		}
		a.defRole = r
	}
	key := cfg.SessionKey
	if len(key) == 0 {
		key = make([]byte, 32)
		_, _ = rand.Read(key)
		a.log.Warn("ZW_SESSION_KEY not set: using a random session key; sessions end on restart and are not shared between nodes")
	}
	if a.seal, err = newSealer(key); err != nil {
		return nil, fmt.Errorf("oidc: %w", err)
	}
	return a, nil
}

// ParseRoleMap parses "claimValue=role,other=role" (claim values may not
// contain commas or '=').
func ParseRoleMap(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		k, v, ok := strings.Cut(p, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if _, valid := httpx.ParseRole(v); !ok || k == "" || !valid {
			return nil, fmt.Errorf("oidc role map entry %q: want claimValue=viewer|editor|admin", p)
		}
		out[k] = v
	}
	return out, nil
}

// Discover fetches (and caches) the provider metadata.
func (a *Authenticator) Discover(ctx context.Context) error {
	_, err := a.provider(ctx)
	return err
}

func (a *Authenticator) provider(ctx context.Context) (*providerMeta, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.meta != nil {
		return a.meta, nil
	}
	if !a.metaErr.IsZero() && a.now().Sub(a.metaErr) < 5*time.Second {
		return nil, errors.New("oidc provider discovery failed recently; retrying shortly")
	}
	var m providerMeta
	err := getJSON(ctx, a.client, strings.TrimRight(a.cfg.Issuer, "/")+"/.well-known/openid-configuration", &m)
	if err == nil && m.Issuer != a.cfg.Issuer {
		err = fmt.Errorf("discovery issuer %q does not match configured issuer %q", m.Issuer, a.cfg.Issuer)
	}
	if err == nil && (m.AuthorizationEndpoint == "" || m.TokenEndpoint == "" || m.JWKSURI == "") {
		err = errors.New("discovery document lacks authorization, token or jwks endpoint")
	}
	if err != nil {
		a.metaErr = a.now()
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	a.meta = &m
	a.jwks = &keySet{url: m.JWKSURI, client: a.client, now: a.now, minRefresh: 10 * time.Second, maxAge: time.Hour}
	return a.meta, nil
}

// ---- session

type session struct {
	Sub    string `json:"s"`
	Email  string `json:"e,omitempty"`
	Role   string `json:"r"`
	Tenant uint32 `json:"t"`
	Exp    int64  `json:"x"`
}

// Session implements httpx.Sessions: the principal from a valid, unexpired
// session cookie.
func (a *Authenticator) Session(r *http.Request) (httpx.Principal, bool) {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return httpx.Principal{}, false
	}
	var s session
	if a.seal.open("session", c.Value, &s) != nil || a.now().Unix() >= s.Exp {
		return httpx.Principal{}, false
	}
	role, ok := httpx.ParseRole(s.Role)
	if !ok || s.Sub == "" || s.Tenant == 0 {
		return httpx.Principal{}, false
	}
	return httpx.Principal{Subject: s.Sub, Email: s.Email, Role: role, Tenant: s.Tenant, Method: httpx.MethodOIDC}, true
}

func (a *Authenticator) setCookie(w http.ResponseWriter, name, value, path string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: path, MaxAge: maxAge,
		HttpOnly: true, Secure: a.cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
}

// ---- handlers

type flow struct {
	State    string `json:"st"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Return   string `json:"r"`
	Exp      int64  `json:"x"`
}

// Login starts the code flow. ?return=<local path> is where to land after.
func (a *Authenticator) Login(w http.ResponseWriter, r *http.Request) {
	m, err := a.provider(r.Context())
	if err != nil {
		a.log.Warn("oidc login: provider unavailable", "err", err)
		page(w, http.StatusBadGateway, "Sign-in is unavailable", "The identity provider could not be reached. Try again shortly.")
		return
	}
	f := flow{State: randomString(32), Nonce: randomString(32), Verifier: randomString(32),
		Return: safeReturn(r.URL.Query().Get("return")), Exp: a.now().Add(flowTTL).Unix()}
	sealed, err := a.seal.seal("flow", f)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	a.setCookie(w, flowCookie, sealed, "/auth/", int(flowTTL/time.Second))
	challenge := sha256.Sum256([]byte(f.Verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {a.cfg.ClientID},
		"redirect_uri":          {a.cfg.RedirectURL},
		"scope":                 {strings.Join(a.cfg.Scopes, " ")},
		"state":                 {f.State},
		"nonce":                 {f.Nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(m.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, m.AuthorizationEndpoint+sep+q.Encode(), http.StatusFound)
}

// Callback finishes the flow: checks state, redeems the code with the PKCE
// verifier, verifies the ID token, maps the role and sets the session.
func (a *Authenticator) Callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		a.log.Info("oidc callback: provider error", "error", e, "description", q.Get("error_description"))
		page(w, http.StatusUnauthorized, "Sign-in failed", "The identity provider answered: "+e)
		return
	}
	c, err := r.Cookie(flowCookie)
	var f flow
	if err != nil || a.seal.open("flow", c.Value, &f) != nil || a.now().Unix() >= f.Exp {
		page(w, http.StatusBadRequest, "Sign-in expired", "The sign-in attempt expired or was started in another browser. Start again.")
		return
	}
	a.setCookie(w, flowCookie, "", "/auth/", -1)
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(f.State)) != 1 || q.Get("code") == "" {
		page(w, http.StatusBadRequest, "Sign-in failed", "The sign-in response did not match this browser's request.")
		return
	}
	p, err := a.redeem(r.Context(), q.Get("code"), f)
	if err != nil {
		var d *deniedError
		if errors.As(err, &d) {
			a.log.Info("oidc sign-in denied", "sub", d.sub, "reason", d.reason)
			page(w, http.StatusForbidden, "Access denied", "Your account is signed in, but it has no ZeroWatch role. Ask an administrator for access.")
			return
		}
		a.log.Warn("oidc callback failed", "err", err)
		page(w, http.StatusUnauthorized, "Sign-in failed", "The identity provider's response could not be verified.")
		return
	}
	exp := a.now().Add(a.cfg.SessionTTL)
	sealed, err := a.seal.seal("session", session{Sub: p.Subject, Email: p.Email, Role: p.Role.String(), Tenant: p.Tenant, Exp: exp.Unix()})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	a.setCookie(w, SessionCookie, sealed, "/", int(a.cfg.SessionTTL/time.Second))
	a.log.Info("oidc sign-in", "sub", p.Subject, "role", p.Role.String(), "tenant", p.Tenant)
	http.Redirect(w, r, f.Return, http.StatusFound)
}

// Logout clears the session cookie and returns to the UI. (The provider's
// own session stays; the next sign-in may be silent.)
func (a *Authenticator) Logout(w http.ResponseWriter, r *http.Request) {
	a.setCookie(w, SessionCookie, "", "/", -1)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/", http.StatusFound)
}

type deniedError struct{ sub, reason string }

func (e *deniedError) Error() string { return "access denied: " + e.reason }

func (a *Authenticator) redeem(ctx context.Context, code string, f flow) (httpx.Principal, error) {
	m, err := a.provider(ctx)
	if err != nil {
		return httpx.Principal{}, err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {a.cfg.RedirectURL}, "code_verifier": {f.Verifier}, "client_id": {a.cfg.ClientID}}
	basic := a.cfg.ClientSecret != "" && (len(m.TokenAuthMethods) == 0 || contains(m.TokenAuthMethods, "client_secret_basic"))
	if a.cfg.ClientSecret != "" && !basic {
		form.Set("client_secret", a.cfg.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return httpx.Principal{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic {
		req.SetBasicAuth(url.QueryEscape(a.cfg.ClientID), url.QueryEscape(a.cfg.ClientSecret))
	}
	res, err := a.client.Do(req)
	if err != nil {
		return httpx.Principal{}, fmt.Errorf("token endpoint: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		return httpx.Principal{}, fmt.Errorf("token endpoint: %s %s", res.Status, e.Error)
	}
	var tok struct {
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.IDToken == "" {
		return httpx.Principal{}, errors.New("token endpoint: no id_token in response")
	}
	claims, err := a.verifyIDToken(ctx, tok.IDToken, f.Nonce)
	if err != nil {
		return httpx.Principal{}, err
	}
	if lookup(claims, a.cfg.RoleClaim) == nil && m.UserinfoEndpoint != "" && tok.AccessToken != "" {
		a.mergeUserinfo(ctx, m.UserinfoEndpoint, tok.AccessToken, claims)
	}
	return a.principal(claims)
}

// verifyIDToken checks signature, iss, aud/azp, exp, nbf, iat and nonce.
func (a *Authenticator) verifyIDToken(ctx context.Context, raw, nonce string) (map[string]any, error) {
	a.mu.Lock()
	ks := a.jwks
	a.mu.Unlock()
	if ks == nil {
		return nil, errors.New("provider not discovered")
	}
	claims, err := verifyJWS(ctx, ks, raw)
	if err != nil {
		return nil, err
	}
	now := a.now()
	if s, _ := claims["iss"].(string); s != a.cfg.Issuer {
		return nil, fmt.Errorf("id token issuer %q mismatch", s)
	}
	auds := strList(claims["aud"])
	if !contains(auds, a.cfg.ClientID) {
		return nil, errors.New("id token audience does not include this client")
	}
	if azp, ok := claims["azp"].(string); ok && azp != a.cfg.ClientID || !ok && len(auds) > 1 {
		return nil, errors.New("id token authorized party mismatch")
	}
	exp, ok := numClaim(claims["exp"])
	if !ok {
		return nil, errors.New("id token has no exp")
	}
	if now.After(time.Unix(exp, 0).Add(clockSkew)) {
		return nil, errors.New("id token expired")
	}
	if nbf, ok := numClaim(claims["nbf"]); ok && now.Add(clockSkew).Before(time.Unix(nbf, 0)) {
		return nil, errors.New("id token not yet valid")
	}
	if iat, ok := numClaim(claims["iat"]); ok && now.Add(clockSkew).Before(time.Unix(iat, 0)) {
		return nil, errors.New("id token issued in the future")
	}
	if n, _ := claims["nonce"].(string); subtle.ConstantTimeCompare([]byte(n), []byte(nonce)) != 1 {
		return nil, errors.New("id token nonce mismatch")
	}
	if s, _ := claims["sub"].(string); s == "" {
		return nil, errors.New("id token has no sub")
	}
	return claims, nil
}

// mergeUserinfo fills claims the ID token lacks (Keycloak, Okta and others
// often put roles/groups only in the access token or userinfo). Failures
// are logged; sign-in then proceeds on the ID token alone.
func (a *Authenticator) mergeUserinfo(ctx context.Context, endpoint, accessToken string, claims map[string]any) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		a.log.Warn("oidc userinfo failed", "err", err)
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		a.log.Warn("oidc userinfo failed", "status", res.Status) // a signed (JWT) userinfo isn't supported
		return
	}
	dec := json.NewDecoder(io.LimitReader(res.Body, 1<<20))
	dec.UseNumber()
	var ui map[string]any
	if dec.Decode(&ui) != nil || ui["sub"] != claims["sub"] {
		a.log.Warn("oidc userinfo ignored: unreadable or sub mismatch")
		return
	}
	for k, v := range ui {
		if _, ok := claims[k]; !ok {
			claims[k] = v
		}
	}
}

func (a *Authenticator) principal(claims map[string]any) (httpx.Principal, error) {
	sub, _ := claims["sub"].(string)
	email, _ := claims["email"].(string)
	if email == "" {
		email, _ = claims["preferred_username"].(string)
	}
	role := httpx.RoleNone
	for _, v := range strList(lookup(claims, a.cfg.RoleClaim)) {
		r, ok := a.roleMap[v]
		if !ok {
			r, _ = httpx.ParseRole(v)
		}
		if r > role {
			role = r
		}
	}
	if role == httpx.RoleNone {
		role = a.defRole
	}
	if role == httpx.RoleNone {
		return httpx.Principal{}, &deniedError{sub, "no claim value in " + a.cfg.RoleClaim + " maps to a role and no default role is set"}
	}
	tenant := a.cfg.DefaultTenant
	if a.cfg.TenantClaim != "" {
		if v := lookup(claims, a.cfg.TenantClaim); v != nil {
			t, ok := tenantValue(v)
			if !ok {
				return httpx.Principal{}, &deniedError{sub, "tenant claim " + a.cfg.TenantClaim + " is not a tenant id"}
			}
			tenant = t
		}
	}
	return httpx.Principal{Subject: sub, Email: email, Role: role, Tenant: tenant, Method: httpx.MethodOIDC}, nil
}

// ---- helpers

// lookup follows a dotted path ("realm_access.roles"). A key containing the
// whole path wins first, for claims named like URLs ("https://x.io/roles").
func lookup(claims map[string]any, path string) any {
	if v, ok := claims[path]; ok {
		return v
	}
	var cur any = claims
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		if cur, ok = m[seg]; !ok {
			return nil
		}
	}
	return cur
}

func strList(v any) []string {
	switch x := v.(type) {
	case string:
		return strings.Fields(x)
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func numClaim(v any) (int64, bool) {
	switch x := v.(type) {
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n, true
		}
		if f, err := x.Float64(); err == nil {
			return int64(f), true
		}
	case float64:
		return int64(x), true
	}
	return 0, false
}

func tenantValue(v any) (uint32, bool) {
	var s string
	switch x := v.(type) {
	case json.Number:
		s = x.String()
	case string:
		s = x
	case float64:
		s = strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || n == 0 {
		return 0, false
	}
	return uint32(n), true
}

// safeReturn keeps post-login redirects on this site: a local absolute path,
// not "//host" or "/\host" (browsers treat both as another origin).
func safeReturn(s string) string {
	if !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") || strings.HasPrefix(s, "/\\") || strings.ContainsAny(s, "\r\n") {
		return "/"
	}
	return s
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func page(w http.ResponseWriter, status int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title>`+
		`<body style="font-family:ui-monospace,monospace;max-width:40rem;margin:4rem auto;padding:0 1rem;background:#0b0b0a;color:#e8e6e1">`+
		`<p style="font-size:.7rem;letter-spacing:.12em;text-transform:uppercase;opacity:.7">ZeroWatch · %d</p><h1 style="font-size:1.4rem;text-transform:uppercase">%s</h1>`+
		`<p>%s</p><p><a style="color:inherit" href="/auth/login">Sign in again</a> · <a style="color:inherit" href="/">Home</a></p></body>`,
		html.EscapeString(title), status, html.EscapeString(title), html.EscapeString(msg))
}
