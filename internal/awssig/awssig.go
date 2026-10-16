// Package awssig signs HTTP requests with AWS Signature Version 4. It is
// shared by the SNS notifier (alarms) and the S3 tier client (tier), and has
// no AWS SDK dependency.
//
// Signed headers: host, plus content-type, content-md5, range and every
// x-amz-* header present on the request. For the s3 service the canonical
// URI is escaped once (the S3 rule); for every other service the escaped path
// is escaped again, as the AWS SDKs do.
package awssig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// UnsignedPayload is the S3 payload hash that skips body hashing.
const UnsignedPayload = "UNSIGNED-PAYLOAD"

// EmptyPayloadHash is the hex SHA-256 of an empty body.
const EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Credentials are an access key pair with an optional session token.
type Credentials struct {
	AccessKeyID  string
	Secret       string
	SessionToken string
}

// Valid reports whether both key parts are set.
func (c Credentials) Valid() bool { return c.AccessKeyID != "" && c.Secret != "" }

// String never prints the secret.
func (c Credentials) String() string { return c.AccessKeyID + ":<redacted>" }

// FromEnv reads <prefix>ACCESS_KEY_ID, <prefix>SECRET_ACCESS_KEY and
// <prefix>SESSION_TOKEN for the first prefix whose key pair is complete
// (e.g. FromEnv("ZW_TIER_", "AWS_")).
func FromEnv(prefixes ...string) Credentials {
	for _, p := range prefixes {
		c := Credentials{AccessKeyID: os.Getenv(p + "ACCESS_KEY_ID"), Secret: os.Getenv(p + "SECRET_ACCESS_KEY"), SessionToken: os.Getenv(p + "SESSION_TOKEN")}
		if c.Valid() {
			return c
		}
	}
	return Credentials{}
}

// PayloadHash returns the hex SHA-256 of body.
func PayloadHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Sign adds X-Amz-Date, X-Amz-Content-Sha256 (for s3), X-Amz-Security-Token
// (with a session token) and Authorization to req. payloadHash is the hex
// SHA-256 of the body or, for s3, UnsignedPayload.
func Sign(req *http.Request, payloadHash string, c Credentials, region, service string, t time.Time) {
	t = t.UTC()
	amzDate, date := t.Format("20060102T150405Z"), t.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	if service == "s3" {
		req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	}
	if c.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", c.SessionToken)
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	vals := map[string]string{"host": host}
	for k, vs := range req.Header {
		lk := strings.ToLower(k)
		if lk == "content-type" || lk == "content-md5" || lk == "range" || strings.HasPrefix(lk, "x-amz-") {
			parts := make([]string, len(vs))
			for i, v := range vs {
				parts[i] = strings.Join(strings.Fields(v), " ")
			}
			vals[lk] = strings.Join(parts, ",")
		}
	}
	names := make([]string, 0, len(vals))
	for k := range vals {
		names = append(names, k)
	}
	sort.Strings(names)
	var hdrs strings.Builder
	for _, k := range names {
		hdrs.WriteString(k + ":" + vals[k] + "\n")
	}
	signed := strings.Join(names, ";")
	uri := Escape(req.URL.EscapedPath(), false)
	if service == "s3" {
		uri = Escape(req.URL.Path, false)
	}
	if uri == "" {
		uri = "/"
	}
	canonical := strings.Join([]string{req.Method, uri, CanonicalQuery(req.URL.RawQuery), hdrs.String(), signed, payloadHash}, "\n")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	crSum := sha256.Sum256([]byte(canonical))
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(crSum[:])
	k := mac(mac(mac(mac([]byte("AWS4"+c.Secret), date), region), service), "aws4_request")
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", c.AccessKeyID, scope, signed, hex.EncodeToString(mac(k, sts))))
}

func mac(k []byte, s string) []byte {
	h := hmac.New(sha256.New, k)
	h.Write([]byte(s))
	return h.Sum(nil)
}

// CanonicalQuery decodes the raw query, re-encodes keys and values with the
// AWS rules and sorts by key then value.
func CanonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	type kv struct{ k, v string }
	var pairs []kv
	for _, p := range strings.Split(raw, "&") {
		if p == "" {
			continue
		}
		k, v, _ := strings.Cut(p, "=")
		if dk, err := url.QueryUnescape(k); err == nil {
			k = dk
		}
		if dv, err := url.QueryUnescape(v); err == nil {
			v = dv
		}
		pairs = append(pairs, kv{Escape(k, true), Escape(v, true)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

// Escape percent-encodes everything except unreserved characters
// (A-Z a-z 0-9 - _ . ~), and '/' unless encodeSlash.
func Escape(s string, encodeSlash bool) string {
	const hexd = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' || c == '/' && !encodeSlash {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexd[c>>4])
		b.WriteByte(hexd[c&15])
	}
	return b.String()
}
