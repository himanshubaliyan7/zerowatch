package authn

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Supported ID token algorithms. Everything else (none, HS*, ...) is refused:
// HMAC with a public client id would let anyone mint tokens.
const (
	algRS256 = "RS256"
	algES256 = "ES256"
)

var errNoKey = errors.New("no matching signing key")

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type verifyKey struct {
	kid string
	alg string // algorithm this key verifies
	pub crypto.PublicKey
}

// keySet caches a provider's JWKS. An unknown kid triggers a refetch (key
// rotation), rate-limited so a flood of forged kids can't hammer the IdP.
type keySet struct {
	url        string
	client     *http.Client
	now        func() time.Time
	minRefresh time.Duration // between kid-miss refetches
	maxAge     time.Duration // refresh even without a miss

	mu      sync.Mutex
	keys    []verifyKey
	fetched time.Time
}

func (ks *keySet) candidates(ctx context.Context, kid, alg string) ([]verifyKey, error) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	now := ks.now()
	stale := ks.fetched.IsZero() || now.Sub(ks.fetched) > ks.maxAge
	if !stale {
		if m := ks.match(kid, alg); len(m) > 0 {
			return m, nil
		}
		if now.Sub(ks.fetched) < ks.minRefresh {
			return nil, errNoKey
		}
	}
	keys, err := fetchJWKS(ctx, ks.client, ks.url)
	if err != nil {
		if m := ks.match(kid, alg); len(m) > 0 { // IdP blip: keep using cached keys
			return m, nil
		}
		return nil, err
	}
	ks.keys, ks.fetched = keys, now
	if m := ks.match(kid, alg); len(m) > 0 {
		return m, nil
	}
	return nil, errNoKey
}

func (ks *keySet) match(kid, alg string) []verifyKey {
	var out []verifyKey
	for _, k := range ks.keys {
		if k.alg == alg && (kid == "" || k.kid == kid) {
			out = append(out, k)
		}
	}
	return out
}

func fetchJWKS(ctx context.Context, c *http.Client, url string) ([]verifyKey, error) {
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := getJSON(ctx, c, url, &doc); err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	var out []verifyKey
	for _, k := range doc.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		vk, err := parseJWK(k)
		if err != nil {
			continue // skip key types we don't verify (e.g. RSA-OAEP enc keys, Ed25519)
		}
		out = append(out, vk)
	}
	if len(out) == 0 {
		return nil, errors.New("jwks: no usable RS256/ES256 signing keys")
	}
	return out, nil
}

func parseJWK(k jwk) (verifyKey, error) {
	switch k.Kty {
	case "RSA":
		if k.Alg != "" && k.Alg != algRS256 {
			return verifyKey{}, errors.New("unsupported alg")
		}
		n, err1 := b64Int(k.N)
		e, err2 := b64Int(k.E)
		if err1 != nil || err2 != nil || !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 {
			return verifyKey{}, errors.New("bad rsa key")
		}
		if n.BitLen() < 2048 {
			return verifyKey{}, errors.New("rsa key shorter than 2048 bits")
		}
		return verifyKey{k.Kid, algRS256, &rsa.PublicKey{N: n, E: int(e.Int64())}}, nil
	case "EC":
		if k.Crv != "P-256" || k.Alg != "" && k.Alg != algES256 {
			return verifyKey{}, errors.New("unsupported curve")
		}
		x, err1 := b64Int(k.X)
		y, err2 := b64Int(k.Y)
		if err1 != nil || err2 != nil || !elliptic.P256().IsOnCurve(x, y) {
			return verifyKey{}, errors.New("bad ec key")
		}
		return verifyKey{k.Kid, algES256, &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}}, nil
	}
	return verifyKey{}, errors.New("unsupported kty")
}

func b64Int(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil || len(b) == 0 {
		return nil, errors.New("bad base64url integer")
	}
	return new(big.Int).SetBytes(b), nil
}

// verifyJWS checks a compact JWS signature against the key set and returns
// the decoded claims. It does not check any claim.
func verifyJWS(ctx context.Context, ks *keySet, token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed token")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("malformed token header")
	}
	var hdr struct {
		Alg  string `json:"alg"`
		Kid  string `json:"kid"`
		Crit []any  `json:"crit"`
	}
	if err := json.Unmarshal(hb, &hdr); err != nil {
		return nil, errors.New("malformed token header")
	}
	if hdr.Alg != algRS256 && hdr.Alg != algES256 {
		return nil, fmt.Errorf("unsupported token algorithm %q", hdr.Alg)
	}
	if len(hdr.Crit) > 0 {
		return nil, errors.New("unsupported critical header")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("malformed token signature")
	}
	keys, err := ks.candidates(ctx, hdr.Kid, hdr.Alg)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	ok := false
	for _, k := range keys {
		switch pub := k.pub.(type) {
		case *rsa.PublicKey:
			ok = rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig) == nil
		case *ecdsa.PublicKey:
			if len(sig) == 64 {
				r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
				ok = ecdsa.Verify(pub, digest[:], r, s)
			}
		}
		if ok {
			break
		}
	}
	if !ok {
		return nil, errors.New("invalid token signature")
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("malformed token payload")
	}
	dec := json.NewDecoder(strings.NewReader(string(pb)))
	dec.UseNumber()
	var claims map[string]any
	if err := dec.Decode(&claims); err != nil {
		return nil, errors.New("malformed token payload")
	}
	return claims, nil
}

func getJSON(ctx context.Context, c *http.Client, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, res.Status)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(v)
}
