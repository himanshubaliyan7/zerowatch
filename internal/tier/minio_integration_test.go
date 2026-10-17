//go:build integration

package tier

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/himanshubaliyan7/zerowatch/internal/awssig"
)

// TestMinIO runs the exactness and retention checks against a real MinIO:
//
//	ZW_TIER_TEST_ENDPOINT=http://w4-tier-minio:9000 ZW_TIER_ACCESS_KEY_ID=... \
//	ZW_TIER_SECRET_ACCESS_KEY=... go test -tags integration ./internal/tier/
func TestMinIO(t *testing.T) {
	ep := os.Getenv("ZW_TIER_TEST_ENDPOINT")
	if ep == "" {
		t.Skip("ZW_TIER_TEST_ENDPOINT not set")
	}
	for _, signed := range []bool{false, true} {
		t.Run(fmt.Sprintf("signPayload=%v", signed), func(t *testing.T) { minioRun(t, ep, signed) })
	}
}

func minioRun(t *testing.T, ep string, signed bool) {
	creds := awssig.FromEnv("ZW_TIER_", "AWS_")
	bucket := fmt.Sprintf("zw-it-%d", time.Now().UnixNano())
	s3, err := NewS3(S3Config{Endpoint: ep, Bucket: bucket, PathStyle: true, Creds: creds, SignPayload: signed})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ { // the second call finds the bucket
		if err := s3.EnsureBucket(ctx); err != nil {
			t.Fatal(err)
		}
	}

	tr := runTieredExactness(t, s3, "it/node1")
	objs, err := s3.List(ctx, "it/")
	if err != nil || len(objs) == 0 {
		t.Fatalf("list: %d %v", len(objs), err)
	}
	t.Logf("minio: %d objects after upload", len(objs))

	tr.cfg.Retention.Default = Policy{RawHot: time.Hour, Raw: 36 * time.Hour, Rollup1m: 36 * time.Hour, Logs: 36 * time.Hour}
	n, err := tr.RunRetention(ctx)
	if err != nil || n == 0 {
		t.Fatalf("retention: %d %v", n, err)
	}
	left, _ := s3.List(ctx, "it/")
	cut := time.Now().Add(-37 * time.Hour).UnixMilli()
	for _, o := range left {
		if hk, ok := parseKey("it/node1", o.Key); !ok || hk.hour < cut {
			t.Errorf("%s survived retention", o.Key)
		}
	}
	t.Logf("minio: retention deleted %d objects, %d left", n, len(left))
	if len(left) != len(objs)-n {
		t.Errorf("deleted %d of %d, %d left", n, len(objs), len(left))
	}
	// Clean up the bucket.
	for _, o := range left {
		_ = s3.Delete(ctx, o.Key)
	}
	if resp, err := s3.do(ctx, http.MethodDelete, s3.objectURL("", nil), nil, nil); err == nil {
		resp.Body.Close()
	}
}
