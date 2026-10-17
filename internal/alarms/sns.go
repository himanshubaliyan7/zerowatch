package alarms

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/awssig"
)

// SNSNotifier publishes transitions to an Amazon SNS topic (Target.URL is
// the topic ARN) with a SigV4-signed Publish call. Credentials come from
// AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (and AWS_SESSION_TOKEN); the
// region is taken from the ARN.
type SNSNotifier struct {
	Client   *http.Client
	Endpoint string // test override for https://sns.<region>.amazonaws.com/
	Now      func() time.Time
}

// snsRegion returns the region of arn:aws:sns:<region>:<account>:<topic>.
func snsRegion(arn string) (string, error) {
	p := strings.Split(arn, ":")
	if len(p) != 6 || p[0] != "arn" || p[2] != "sns" || p[3] == "" {
		return "", fmt.Errorf("not an SNS topic ARN: %q", arn)
	}
	return p[3], nil
}

// Notify implements Notifier.
func (n SNSNotifier) Notify(ctx context.Context, t Target, tr Transition) error {
	region, err := snsRegion(t.URL)
	if err != nil {
		return err
	}
	key, secret := os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY")
	if key == "" || secret == "" {
		return fmt.Errorf("sns: AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY must be set")
	}
	ep := n.Endpoint
	if ep == "" {
		ep = "https://sns." + region + ".amazonaws.com/"
	}
	subject := fmt.Sprintf("ZeroWatch: %s is %s", tr.RuleName, tr.To)
	if len(subject) > 100 { // SNS limit
		subject = subject[:100]
	}
	form := url.Values{"Action": {"Publish"}, "Version": {"2010-03-31"}, "TopicArn": {t.URL}, "Subject": {subject},
		"Message": {fmt.Sprintf("%s moved %s -> %s at %s\n%s", tr.RuleName, tr.From, tr.To, tr.Timestamp.UTC().Format(time.RFC3339), tr.Reason)}}
	body := form.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	now := time.Now
	if n.Now != nil {
		now = n.Now
	}
	signSigV4(req, body, key, secret, os.Getenv("AWS_SESSION_TOKEN"), region, "sns", now().UTC())
	resp, err := n.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("sns publish: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b[:min(len(b), 300)])))
	}
	return nil
}

// signSigV4 signs req (body already set) with AWS Signature Version 4; see
// awssig.Sign for the headers it signs.
func signSigV4(req *http.Request, body, key, secret, token, region, service string, t time.Time) {
	awssig.Sign(req, awssig.PayloadHash([]byte(body)), awssig.Credentials{AccessKeyID: key, Secret: secret, SessionToken: token}, region, service, t)
}
