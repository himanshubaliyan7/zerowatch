package httpx

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AWS Signature Version 4 verification for the CloudWatch-compatible
// endpoints. Supports the Authorization header form and presigned query-string
// auth (X-Amz-Algorithm/X-Amz-Credential/X-Amz-Signature...). The payload hash
// is always computed over the actual body, so UNSIGNED-PAYLOAD and the
// aws-chunked STREAMING-* payload modes are rejected (CloudWatch SDKs never use
// them). Canonical URIs are escaped the non-S3 way (the escaped path is
// escaped again) without dot-segment normalisation, matching the AWS SDKs.

const (
	sigV4Algorithm = "AWS4-HMAC-SHA256"
	sigV4Terminal  = "aws4_request"
	amzDateFormat  = "20060102T150405Z"
	// MaxSigV4Body caps the body buffered for payload hashing; it matches the
	// CloudWatch handlers' limit (ingest.MaxCWBody).
	MaxSigV4Body = 1 << 20
	// MaxSigV4Skew is the allowed clock skew between X-Amz-Date and now.
	MaxSigV4Skew  = 15 * time.Minute
	maxPresignTTL = 7 * 24 * time.Hour
)

// AWSCredential is an access key id / secret pair mapped to a tenant.
// String and LogValue redact the secret so it can't leak into logs.
type AWSCredential struct {
	AccessKeyID string
	Secret      string
	Tenant      uint32
}

func (c AWSCredential) String() string { return c.AccessKeyID + ":<redacted>" }

// LogValue implements slog.LogValuer.
func (c AWSCredential) LogValue() slog.Value {
	return slog.GroupValue(slog.String("accessKeyId", c.AccessKeyID), slog.Uint64("tenant", uint64(c.Tenant)))
}

// SetAWSCredentials enables SigV4 verification: requests signed with
// AWS4-HMAC-SHA256 must then carry a known access key id and a valid
// signature. Without credentials SigV4 requests fall back to mapping the
// access key id through the API keys, unverified (legacy behaviour).
func (k *KeyStore) SetAWSCredentials(creds []AWSCredential) {
	if len(creds) == 0 {
		k.aws = nil
		return
	}
	k.aws = map[string]AWSCredential{}
	for _, c := range creds {
		k.aws[c.AccessKeyID] = c
	}
}

// sigV4Services are the signing scopes accepted (CloudWatch and CloudWatch Logs).
var sigV4Services = map[string]bool{"monitoring": true, "logs": true}

// sigV4Request is a parsed SigV4 authentication (header or presigned).
type sigV4Request struct {
	accessKeyID   string
	date          string // yyyymmdd from the credential scope
	region        string
	service       string
	signedHeaders []string
	signature     string
	amzDate       time.Time
	presigned     bool
	expires       time.Duration
}

// sigV4Error is an authentication failure rendered in AWS error style.
type sigV4Error struct {
	status int
	code   string
	msg    string
}

func (e *sigV4Error) Error() string { return e.code + ": " + e.msg }

func sigErr(status int, code, format string, a ...any) *sigV4Error {
	return &sigV4Error{status, code, fmt.Sprintf(format, a...)}
}

// isSigV4 reports whether r carries SigV4 authentication.
func isSigV4(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Authorization"), sigV4Algorithm) ||
		r.URL.Query().Get("X-Amz-Algorithm") != ""
}

// parseSigV4 extracts the SigV4 parameters from the Authorization header or the
// presigned query string.
func parseSigV4(r *http.Request) (*sigV4Request, *sigV4Error) {
	s := &sigV4Request{}
	var credential, signedHeaders, dateStr string
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, sigV4Algorithm) {
		rest := strings.TrimPrefix(a, sigV4Algorithm)
		if rest == "" || rest[0] != ' ' {
			return nil, sigErr(400, "IncompleteSignature", "unsupported authorization algorithm")
		}
		for _, part := range strings.Split(rest, ",") {
			k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
			switch k {
			case "Credential":
				credential = v
			case "SignedHeaders":
				signedHeaders = v
			case "Signature":
				s.signature = v
			}
		}
		dateStr = r.Header.Get("X-Amz-Date")
		if dateStr == "" {
			return nil, sigErr(400, "IncompleteSignature", "X-Amz-Date header is required")
		}
	} else {
		q := r.URL.Query()
		if q.Get("X-Amz-Algorithm") != sigV4Algorithm {
			return nil, sigErr(400, "IncompleteSignature", "unsupported X-Amz-Algorithm")
		}
		s.presigned = true
		credential, signedHeaders, s.signature, dateStr = q.Get("X-Amz-Credential"), q.Get("X-Amz-SignedHeaders"), q.Get("X-Amz-Signature"), q.Get("X-Amz-Date")
		exp, err := strconv.Atoi(q.Get("X-Amz-Expires"))
		if err != nil || exp <= 0 || time.Duration(exp)*time.Second > maxPresignTTL {
			return nil, sigErr(400, "AuthorizationQueryParametersError", "X-Amz-Expires must be between 1 and 604800 seconds")
		}
		s.expires = time.Duration(exp) * time.Second
	}
	if credential == "" || signedHeaders == "" || s.signature == "" {
		return nil, sigErr(400, "IncompleteSignature", "Credential, SignedHeaders and Signature are required")
	}
	scope := strings.Split(credential, "/")
	if len(scope) != 5 || scope[0] == "" || scope[4] != sigV4Terminal {
		return nil, sigErr(400, "IncompleteSignature", "malformed credential scope")
	}
	s.accessKeyID, s.date, s.region, s.service = scope[0], scope[1], scope[2], scope[3]
	t, err := time.Parse(amzDateFormat, dateStr)
	if err != nil {
		return nil, sigErr(400, "IncompleteSignature", "X-Amz-Date must be in ISO8601 basic format (yyyyMMddTHHmmssZ)")
	}
	s.amzDate = t
	if !strings.HasPrefix(dateStr, s.date) {
		return nil, sigErr(403, "SignatureDoesNotMatch", "credential scope date does not match X-Amz-Date")
	}
	s.signedHeaders = strings.Split(signedHeaders, ";")
	if !sort.StringsAreSorted(s.signedHeaders) || !contains(s.signedHeaders, "host") {
		return nil, sigErr(403, "SignatureDoesNotMatch", "SignedHeaders must be sorted and include host")
	}
	return s, nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// verifySigV4 authenticates r against creds. On success the body is buffered
// and replaced, so the handler can still read it.
func verifySigV4(r *http.Request, creds map[string]AWSCredential, now time.Time) (AWSCredential, *sigV4Error) {
	s, e := parseSigV4(r)
	if e != nil {
		return AWSCredential{}, e
	}
	cred, ok := creds[s.accessKeyID]
	if !ok {
		return AWSCredential{}, sigErr(403, "InvalidClientTokenId", "the security token included in the request is invalid")
	}
	if !sigV4Services[s.service] {
		return AWSCredential{}, sigErr(403, "SignatureDoesNotMatch", "credential should be scoped to a valid service (monitoring or logs)")
	}
	if s.presigned {
		if now.Before(s.amzDate.Add(-MaxSigV4Skew)) || now.After(s.amzDate.Add(s.expires)) {
			return AWSCredential{}, sigErr(400, "RequestExpired", "request has expired")
		}
	} else if d := now.Sub(s.amzDate); d > MaxSigV4Skew || d < -MaxSigV4Skew {
		return AWSCredential{}, sigErr(400, "RequestExpired", "signature expired: X-Amz-Date is more than 15 minutes from the server time")
	}
	if ph := r.Header.Get("X-Amz-Content-Sha256"); ph == "UNSIGNED-PAYLOAD" || strings.HasPrefix(ph, "STREAMING-") {
		return AWSCredential{}, sigErr(400, "InvalidRequest", "unsigned and streaming payloads are not supported")
	}
	body, e := bufferBody(r)
	if e != nil {
		return AWSCredential{}, e
	}
	sum := sha256.Sum256(body)
	cr := canonicalRequest(r, s.signedHeaders, hex.EncodeToString(sum[:]))
	want := hex.EncodeToString(signV4(cred.Secret, s.date, s.region, s.service, stringToSign(s.amzDate, s.date+"/"+s.region+"/"+s.service+"/"+sigV4Terminal, cr)))
	if subtle.ConstantTimeCompare([]byte(want), []byte(strings.ToLower(s.signature))) != 1 {
		return AWSCredential{}, sigErr(403, "SignatureDoesNotMatch", "the request signature we calculated does not match the signature you provided")
	}
	return cred, nil
}

// bufferBody reads the body (capped at MaxSigV4Body) and replaces r.Body with
// a re-readable copy.
func bufferBody(r *http.Request) ([]byte, *sigV4Error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, MaxSigV4Body+1))
	_ = r.Body.Close()
	if err != nil {
		return nil, sigErr(400, "InvalidRequest", "failed to read body")
	}
	if len(b) > MaxSigV4Body {
		return nil, sigErr(413, "RequestEntityTooLarge", fmt.Sprintf("body exceeds %d bytes", MaxSigV4Body))
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	return b, nil
}

// canonicalRequest builds the SigV4 canonical request.
func canonicalRequest(r *http.Request, signedHeaders []string, payloadHash string) string {
	var b strings.Builder
	b.WriteString(r.Method)
	b.WriteByte('\n')
	uri := awsEscape(r.URL.EscapedPath(), false)
	if uri == "" {
		uri = "/"
	}
	b.WriteString(uri)
	b.WriteByte('\n')
	b.WriteString(canonicalQuery(r.URL.RawQuery))
	b.WriteByte('\n')
	for _, h := range signedHeaders {
		b.WriteString(h)
		b.WriteByte(':')
		b.WriteString(canonicalHeaderValue(r, h))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(strings.Join(signedHeaders, ";"))
	b.WriteByte('\n')
	b.WriteString(payloadHash)
	return b.String()
}

func canonicalHeaderValue(r *http.Request, name string) string {
	var vals []string
	switch name {
	case "host":
		vals = []string{r.Host}
	case "content-length":
		vals = r.Header.Values("Content-Length")
		if len(vals) == 0 && r.ContentLength >= 0 {
			vals = []string{strconv.FormatInt(r.ContentLength, 10)}
		}
	default:
		vals = r.Header.Values(name)
	}
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = strings.Join(strings.Fields(v), " ")
	}
	return strings.Join(out, ",")
}

// canonicalQuery decodes the raw query, drops X-Amz-Signature, re-encodes
// keys and values with AWS rules and sorts by key then value.
func canonicalQuery(raw string) string {
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
		if k == "X-Amz-Signature" {
			continue
		}
		pairs = append(pairs, kv{awsEscape(k, true), awsEscape(v, true)})
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

// awsEscape percent-encodes everything except unreserved characters
// (A-Z a-z 0-9 - _ . ~), and '/' unless encodeSlash.
func awsEscape(s string, encodeSlash bool) string {
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

func stringToSign(t time.Time, scope, canonical string) string {
	h := sha256.Sum256([]byte(canonical))
	return sigV4Algorithm + "\n" + t.UTC().Format(amzDateFormat) + "\n" + scope + "\n" + hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// signingKey derives the SigV4 signing key for date/region/service.
func signingKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, sigV4Terminal)
}

func signV4(secret, date, region, service, sts string) []byte {
	return hmacSHA256(signingKey(secret, date, region, service), sts)
}

// writeSigV4Error renders e in the wire protocol the caller speaks: awsJson
// (X-Amz-Target), awsQuery XML (form posts / Action=), otherwise the httpx
// envelope.
func writeSigV4Error(w http.ResponseWriter, r *http.Request, e *sigV4Error) {
	switch {
	case r.Header.Get("X-Amz-Target") != "":
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		w.Header().Set("X-Amzn-ErrorType", e.code)
		w.WriteHeader(e.status)
		_ = json.NewEncoder(w).Encode(map[string]string{"__type": e.code, "message": e.msg})
	case strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") || r.URL.Query().Get("Action") != "":
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(e.status)
		fmt.Fprintf(w, `<ErrorResponse xmlns="http://monitoring.amazonaws.com/doc/2010-08-01/"><Error><Type>Sender</Type><Code>%s</Code><Message>%s</Message></Error><RequestId>%s</RequestId></ErrorResponse>`,
			xmlText(e.code), xmlText(e.msg), xmlText(RequestID(r.Context())))
	default:
		WriteError(w, r, &APIError{Status: e.status, Code: e.code, Message: e.msg})
	}
}

func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
