package awssig

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zerowatch/zerowatch/internal/httpx"
)

var exampleCreds = Credentials{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", Secret: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}

// The S3 examples from the AWS "Signature Calculations for the Authorization
// Header" documentation (GET object with a Range, and ListObjects).
func TestS3DocumentationVectors(t *testing.T) {
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name, url, rng, want string
	}{
		{"get object", "https://examplebucket.s3.amazonaws.com/test.txt", "bytes=0-9", "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"},
		{"list objects", "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J", "", "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(http.MethodGet, c.url, nil)
		if c.rng != "" {
			req.Header.Set("Range", c.rng)
		}
		Sign(req, EmptyPayloadHash, exampleCreds, "us-east-1", "s3", at)
		got := req.Header.Get("Authorization")
		if !strings.HasSuffix(got, "Signature="+c.want) {
			t.Errorf("%s: %s", c.name, got)
		}
	}
}

// Non-S3 signatures are checked against ZeroWatch's own SigV4 verifier
// (which follows the AWS test vectors); it only accepts CloudWatch scopes.
func TestSignVerifiesAgainstHTTPX(t *testing.T) {
	ks := httpx.NewKeyStore(nil)
	ks.SetAWSCredentials([]httpx.AWSCredential{{AccessKeyID: exampleCreds.AccessKeyID, Secret: exampleCreds.Secret, Tenant: 1}})
	ok := false
	srv := httptest.NewServer(httpx.Auth(ks, func(*http.Request) bool { return false })(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ok = true })))
	defer srv.Close()
	for _, c := range []struct{ path, token string }{{"/", ""}, {"/a%20b/c%2Fd?x=1&a=2%203", "session-token"}} {
		ok = false
		body := "Action=PutMetricData&Namespace=x"
		req, _ := http.NewRequest(http.MethodPost, srv.URL+c.path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
		Sign(req, PayloadHash([]byte(body)), Credentials{AccessKeyID: exampleCreds.AccessKeyID, Secret: exampleCreds.Secret, SessionToken: c.token}, "us-east-1", "monitoring", time.Now())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !ok || resp.StatusCode != 200 {
			t.Fatalf("%q: signature refused: %d %s", c.path, resp.StatusCode, b)
		}
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("ZW_TIER_ACCESS_KEY_ID", "")
	t.Setenv("ZW_TIER_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "AK")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "SK")
	if c := FromEnv("ZW_TIER_", "AWS_"); c.AccessKeyID != "AK" || c.Secret != "SK" {
		t.Fatalf("fallback: %v", c)
	}
	t.Setenv("ZW_TIER_ACCESS_KEY_ID", "ZK")
	t.Setenv("ZW_TIER_SECRET_ACCESS_KEY", "ZS")
	if c := FromEnv("ZW_TIER_", "AWS_"); c.AccessKeyID != "ZK" || c.String() != "ZK:<redacted>" {
		t.Fatalf("prefix order: %v", c)
	}
}
