package alarms

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/httpx"
)

// The signer is checked against ZeroWatch's own SigV4 verifier (which follows
// the AWS test vectors); it only accepts CloudWatch scopes, hence "monitoring".
func TestSignSigV4Verifies(t *testing.T) {
	ks := httpx.NewKeyStore(nil)
	ks.SetAWSCredentials([]httpx.AWSCredential{{AccessKeyID: "AKIDEXAMPLE", Secret: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", Tenant: 1}})
	ok := false
	srv := httptest.NewServer(httpx.Auth(ks, func(*http.Request) bool { return false })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ok = true })))
	defer srv.Close()
	for _, token := range []string{"", "session-token"} {
		ok = false
		body := "Action=Publish&Message=hi"
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
		signSigV4(req, body, "AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", token, "us-east-1", "monitoring", time.Now().UTC())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !ok || resp.StatusCode != 200 {
			t.Fatalf("token %q: signature refused: %d %s", token, resp.StatusCode, b)
		}
	}
}

func TestSNSNotifier(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secretsecretsecret")
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(b))
		if !strings.Contains(r.Header.Get("Authorization"), "/eu-west-1/sns/aws4_request") {
			w.WriteHeader(403)
		}
	}))
	defer srv.Close()
	n := SNSNotifier{Client: srv.Client(), Endpoint: srv.URL + "/"}
	tr := Transition{RuleName: "disk", From: StateOK, To: StateAlarm, Reason: "forecast > 90", Timestamp: time.Now()}
	if err := n.Notify(context.Background(), Target{Type: "sns", URL: "arn:aws:sns:eu-west-1:123456789012:alerts"}, tr); err != nil {
		t.Fatal(err)
	}
	if form.Get("Action") != "Publish" || form.Get("TopicArn") != "arn:aws:sns:eu-west-1:123456789012:alerts" || !strings.Contains(form.Get("Message"), "forecast > 90") {
		t.Fatalf("form %v", form)
	}
	if err := n.Notify(context.Background(), Target{Type: "sns", URL: "not-an-arn"}, tr); err == nil {
		t.Error("bad ARN should fail")
	}
}
