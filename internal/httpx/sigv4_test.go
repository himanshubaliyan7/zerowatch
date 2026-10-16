package httpx

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Vectors from the AWS SigV4 test suite (aws-sig-v4-test-suite) and the IAM
// ListUsers example in the AWS General Reference.
const (
	tvKeyID  = "AKIDEXAMPLE"
	tvSecret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	tvDate   = "20150830T123600Z"
	emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

func tvTime(t *testing.T) time.Time {
	ts, err := time.Parse(amzDateFormat, tvDate)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func TestSigV4SuiteVectors(t *testing.T) {
	cases := []struct {
		name, method, target, body, service string
		headers                             map[string]string
		signed                              []string
		canonical, sig                      string
	}{
		{
			name: "get-vanilla", method: "GET", target: "/", service: "service",
			signed:    []string{"host", "x-amz-date"},
			canonical: "GET\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + emptySHA,
			sig:       "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31",
		},
		{
			name: "get-vanilla-query-order-key-case", method: "GET", target: "/?Param2=value2&Param1=value1", service: "service",
			signed:    []string{"host", "x-amz-date"},
			canonical: "GET\n/\nParam1=value1&Param2=value2\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + emptySHA,
			sig:       "b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500",
		},
		{
			name: "iam-listusers", method: "GET", target: "/?Action=ListUsers&Version=2010-05-08", service: "iam",
			headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded; charset=utf-8"},
			signed:  []string{"content-type", "host", "x-amz-date"},
			canonical: "GET\n/\nAction=ListUsers&Version=2010-05-08\ncontent-type:application/x-www-form-urlencoded; charset=utf-8\n" +
				"host:iam.amazonaws.com\nx-amz-date:20150830T123600Z\n\ncontent-type;host;x-amz-date\n" + emptySHA,
			sig: "5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.target, strings.NewReader(c.body))
			req.Host = "example.amazonaws.com"
			if c.service == "iam" {
				req.Host = "iam.amazonaws.com"
			}
			req.Header.Set("X-Amz-Date", tvDate)
			for k, v := range c.headers {
				req.Header.Set(k, v)
			}
			cr := canonicalRequest(req, c.signed, sha(c.body))
			if cr != c.canonical {
				t.Fatalf("canonical request mismatch:\n got %q\nwant %q", cr, c.canonical)
			}
			sig := hex.EncodeToString(signV4(tvSecret, "20150830", "us-east-1", c.service,
				stringToSign(tvTime(t), "20150830/us-east-1/"+c.service+"/aws4_request", cr)))
			if sig != c.sig {
				t.Fatalf("signature %s want %s", sig, c.sig)
			}
		})
	}
	// Signing key from the AWS General Reference "derive a signing key" example.
	if k := hex.EncodeToString(signingKey(tvSecret, "20150830", "us-east-1", "iam")); k != "c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9" {
		t.Fatalf("signing key %s", k)
	}
}

// The full verifier accepts the official get-vanilla request byte-for-byte.
func TestVerifySigV4SuiteRequest(t *testing.T) {
	sigV4Services["service"] = true
	defer delete(sigV4Services, "service")
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "example.amazonaws.com"
	req.Header.Set("X-Amz-Date", tvDate)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31")
	creds := map[string]AWSCredential{tvKeyID: {tvKeyID, tvSecret, 5}}
	if c, e := verifySigV4(req, creds, tvTime(t).Add(time.Minute)); e != nil || c.Tenant != 5 {
		t.Fatalf("verify: %v %v", c, e)
	}
}

func TestAWSEscape(t *testing.T) {
	if got := awsEscape("a b/c~d+e=é", true); got != "a%20b%2Fc~d%2Be%3D%C3%A9" {
		t.Fatal(got)
	}
	if got := canonicalQuery("b=2&a=x%20y&a=1&X-Amz-Signature=zz&c"); got != "a=1&a=x%20y&b=2&c=" {
		t.Fatal(got)
	}
}

// signRequest is a minimal SigV4 client signer (header form) for the tests.
func signRequest(t *testing.T, r *http.Request, body, keyID, secret, service string, at time.Time) {
	t.Helper()
	date := at.UTC().Format(amzDateFormat)
	r.Header.Set("X-Amz-Date", date)
	r.Header.Set("X-Amz-Content-Sha256", sha(body))
	signed := []string{"content-type", "host", "x-amz-content-sha256", "x-amz-date"}
	if r.Header.Get("X-Amz-Target") != "" {
		signed = append(signed, "x-amz-target")
	}
	scope := date[:8] + "/us-west-2/" + service + "/aws4_request"
	cr := canonicalRequest(r, signed, sha(body))
	sig := hex.EncodeToString(signV4(secret, date[:8], "us-west-2", service, stringToSign(at, scope, cr)))
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+keyID+"/"+scope+", SignedHeaders="+strings.Join(signed, ";")+", Signature="+sig)
}

func TestAuthSigV4EndToEnd(t *testing.T) {
	ks := NewKeyStore(map[string]uint32{"legacy-key-0123456789": 2})
	ks.SetAWSCredentials([]AWSCredential{{AccessKeyID: tvKeyID, Secret: tvSecret, Tenant: 42}})
	var gotTenant uint32
	var gotBody string
	h := Auth(ks, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant = Tenant(r.Context())
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
	}))
	const body = `{"Namespace":"App","MetricData":[{"MetricName":"Latency","Value":12.5}]}`
	newReq := func(b string) *http.Request {
		r := httptest.NewRequest("POST", "http://zw.local:8080/cloudwatch", strings.NewReader(b))
		r.Header.Set("Content-Type", "application/x-amz-json-1.0")
		r.Header.Set("X-Amz-Target", "GraniteServiceVersion20100801.PutMetricData")
		return r
	}
	serve := func(r *http.Request) (*httptest.ResponseRecorder, string) {
		gotTenant, gotBody = 0, ""
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var e map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		return rec, e["__type"]
	}

	r := newReq(body)
	signRequest(t, r, body, tvKeyID, tvSecret, "monitoring", time.Now())
	if rec, _ := serve(r); rec.Code != 200 || gotTenant != 42 || gotBody != body {
		t.Fatalf("valid: %d tenant=%d body=%q %s", rec.Code, gotTenant, gotBody, rec.Body)
	}

	r = newReq("") // logs service, empty body
	signRequest(t, r, "", tvKeyID, tvSecret, "logs", time.Now())
	if rec, _ := serve(r); rec.Code != 200 || gotTenant != 42 {
		t.Fatalf("logs: %d", rec.Code)
	}

	r = newReq(body)
	signRequest(t, r, body, tvKeyID, tvSecret, "monitoring", time.Now())
	r.Body = io.NopCloser(strings.NewReader(strings.Replace(body, "12.5", "99.0", 1)))
	if rec, code := serve(r); rec.Code != 403 || code != "SignatureDoesNotMatch" || gotTenant != 0 {
		t.Fatalf("tampered body: %d %s", rec.Code, code)
	}

	r = newReq(body)
	signRequest(t, r, body, tvKeyID, tvSecret, "monitoring", time.Now())
	r.Header.Set("X-Amz-Target", "Logs_20140328.PutLogEvents") // tampered signed header
	if rec, code := serve(r); rec.Code != 403 || code != "SignatureDoesNotMatch" {
		t.Fatalf("tampered header: %d %s", rec.Code, code)
	}

	r = newReq(body)
	signRequest(t, r, body, tvKeyID, "the-wrong-secret-0123456789", "monitoring", time.Now())
	if rec, code := serve(r); rec.Code != 403 || code != "SignatureDoesNotMatch" {
		t.Fatalf("wrong secret: %d %s", rec.Code, code)
	}

	r = newReq(body)
	signRequest(t, r, body, "AKIDUNKNOWN", tvSecret, "monitoring", time.Now())
	if rec, code := serve(r); rec.Code != 403 || code != "InvalidClientTokenId" {
		t.Fatalf("unknown key: %d %s", rec.Code, code)
	}

	r = newReq(body)
	signRequest(t, r, body, tvKeyID, tvSecret, "monitoring", time.Now().Add(-16*time.Minute))
	if rec, code := serve(r); rec.Code != 400 || code != "RequestExpired" {
		t.Fatalf("stale date: %d %s", rec.Code, code)
	}

	r = newReq(body)
	signRequest(t, r, body, tvKeyID, tvSecret, "s3", time.Now())
	if rec, code := serve(r); rec.Code != 403 || code != "SignatureDoesNotMatch" {
		t.Fatalf("wrong service: %d %s", rec.Code, code)
	}

	big := strings.Repeat("x", MaxSigV4Body+1)
	r = newReq(big)
	signRequest(t, r, big, tvKeyID, tvSecret, "monitoring", time.Now())
	if rec, code := serve(r); rec.Code != 413 || code != "RequestEntityTooLarge" {
		t.Fatalf("oversize: %d %s", rec.Code, code)
	}

	// awsQuery (form) callers get the XML error shape.
	form := "Action=PutMetricData&Namespace=App"
	r = httptest.NewRequest("POST", "http://zw.local/cloudwatch", strings.NewReader(form))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	signRequest(t, r, form, tvKeyID, "the-wrong-secret-0123456789", "monitoring", time.Now())
	if rec, _ := serve(r); rec.Code != 403 || !strings.Contains(rec.Body.String(), "<Code>SignatureDoesNotMatch</Code>") {
		t.Fatalf("xml error: %d %s", rec.Code, rec.Body)
	}
	r = httptest.NewRequest("POST", "http://zw.local/cloudwatch", strings.NewReader(form))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	signRequest(t, r, form, tvKeyID, tvSecret, "monitoring", time.Now())
	if rec, _ := serve(r); rec.Code != 200 || gotBody != form {
		t.Fatalf("form: %d %q", rec.Code, gotBody)
	}

	// Non-SigV4 API keys keep working alongside AWS credentials.
	r = httptest.NewRequest("GET", "/api/x", nil)
	r.Header.Set("Authorization", "Bearer legacy-key-0123456789")
	if rec, _ := serve(r); rec.Code != 200 || gotTenant != 2 {
		t.Fatalf("bearer: %d %d", rec.Code, gotTenant)
	}
}

func TestAuthSigV4Presigned(t *testing.T) {
	ks := NewKeyStore(nil)
	ks.SetAWSCredentials([]AWSCredential{{AccessKeyID: tvKeyID, Secret: tvSecret, Tenant: 9}})
	if !ks.Enabled() {
		t.Fatal("AWS credentials alone should enable auth")
	}
	var gotTenant uint32
	h := Auth(ks, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gotTenant = Tenant(r.Context()); w.WriteHeader(200) }))
	presign := func(at time.Time, expires int, secret string) *http.Request {
		date := at.UTC().Format(amzDateFormat)
		scope := date[:8] + "/eu-central-1/monitoring/aws4_request"
		q := url.Values{}
		q.Set("Action", "ListMetrics")
		q.Set("X-Amz-Algorithm", sigV4Algorithm)
		q.Set("X-Amz-Credential", tvKeyID+"/"+scope)
		q.Set("X-Amz-Date", date)
		q.Set("X-Amz-Expires", strconv.Itoa(expires))
		q.Set("X-Amz-SignedHeaders", "host")
		r := httptest.NewRequest("GET", "http://zw.local/cloudwatch?"+strings.ReplaceAll(q.Encode(), "+", "%20"), nil)
		cr := canonicalRequest(r, []string{"host"}, emptySHA)
		sig := hex.EncodeToString(signV4(secret, date[:8], "eu-central-1", "monitoring", stringToSign(at, scope, cr)))
		r.URL.RawQuery += "&X-Amz-Signature=" + sig
		return r
	}
	for _, c := range []struct {
		name   string
		r      *http.Request
		status int
	}{
		{"valid", presign(time.Now().Add(-time.Minute), 300, tvSecret), 200},
		{"expired", presign(time.Now().Add(-10*time.Minute), 300, tvSecret), 400},
		{"wrong secret", presign(time.Now(), 300, "the-wrong-secret-0123456789"), 403},
	} {
		gotTenant = 0
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, c.r)
		if rec.Code != c.status || (c.status == 200) != (gotTenant == 9) {
			t.Fatalf("%s: %d tenant=%d %s", c.name, rec.Code, gotTenant, rec.Body)
		}
	}
}

// Without AWS credentials the legacy behaviour is unchanged: the access key id
// is looked up as an API key and the signature is not checked.
func TestAuthSigV4LegacyUnverified(t *testing.T) {
	ks := NewKeyStore(map[string]uint32{"legacy-key-0123456789": 4})
	var gotTenant uint32
	h := Auth(ks, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gotTenant = Tenant(r.Context()) }))
	r := httptest.NewRequest("POST", "/cloudwatch", nil)
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=legacy-key-0123456789/20260101/us-east-1/monitoring/aws4_request, SignedHeaders=host, Signature=bogus")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 || gotTenant != 4 {
		t.Fatalf("%d %d", rec.Code, gotTenant)
	}
}
