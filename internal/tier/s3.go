package tier

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zerowatch/zerowatch/internal/awssig"
)

// ErrNotFound is returned by Get for a missing key.
var ErrNotFound = errors.New("tier: object not found")

// S3Config configures the S3 client.
type S3Config struct {
	// Endpoint is the service URL, e.g. https://s3.eu-west-1.amazonaws.com
	// or http://minio:9000. Empty means https://s3.<region>.amazonaws.com.
	Endpoint string
	Bucket   string
	Region   string // default us-east-1 (MinIO accepts it)
	// PathStyle addresses <endpoint>/<bucket>/<key> (MinIO); otherwise
	// <bucket>.<endpoint host>/<key>.
	PathStyle bool
	Creds     awssig.Credentials
	// SignPayload hashes request bodies instead of sending UNSIGNED-PAYLOAD.
	SignPayload bool
	Client      *http.Client
	Now         func() time.Time
}

// ObjectInfo is one LIST entry.
type ObjectInfo struct {
	Key  string
	Size int64
}

// S3 is a minimal S3 client: PUT, GET (with Range), LIST v2 and DELETE.
type S3 struct {
	cfg  S3Config
	base *url.URL
}

// NewS3 validates cfg and returns a client.
func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("tier: bucket is required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://s3." + cfg.Region + ".amazonaws.com"
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 60 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	u, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("tier: bad S3 endpoint %q", cfg.Endpoint)
	}
	if !cfg.PathStyle {
		u.Host = cfg.Bucket + "." + u.Host
	}
	return &S3{cfg: cfg, base: u}, nil
}

// Bucket returns the configured bucket.
func (s *S3) Bucket() string { return s.cfg.Bucket }

func (s *S3) objectURL(key string, q url.Values) *url.URL {
	u := *s.base
	p := u.Path
	if s.cfg.PathStyle {
		p += "/" + s.cfg.Bucket
	}
	if key != "" || !s.cfg.PathStyle {
		p += "/" + key
	}
	u.Path = p
	u.RawPath = awssig.Escape(p, false)
	if q != nil {
		// Encode the query with the AWS rules so the wire form equals the
		// canonical form.
		u.RawQuery = awssig.CanonicalQuery(q.Encode())
	}
	return &u
}

func (s *S3) do(ctx context.Context, method string, u *url.URL, body []byte, hdr http.Header) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	if s.cfg.Creds.Valid() {
		ph := awssig.UnsignedPayload
		if s.cfg.SignPayload || body == nil {
			ph = awssig.PayloadHash(body)
		}
		awssig.Sign(req, ph, s.cfg.Creds, s.cfg.Region, "s3", s.cfg.Now())
	}
	return s.cfg.Client.Do(req)
}

func s3Error(op, key string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	msg := strings.TrimSpace(string(b))
	if xml.Unmarshal(b, &e) == nil && e.Code != "" {
		msg = e.Code + ": " + e.Message
	}
	return fmt.Errorf("tier: s3 %s %s: HTTP %d %s", op, key, resp.StatusCode, msg)
}

// Put writes an object.
func (s *S3) Put(ctx context.Context, key string, body []byte, contentType string) error {
	h := http.Header{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	if body == nil {
		body = []byte{}
	}
	resp, err := s.do(ctx, http.MethodPut, s.objectURL(key, nil), body, h)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return s3Error("PUT", key, resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// Get reads a whole object.
func (s *S3) Get(ctx context.Context, key string) ([]byte, error) { return s.GetRange(ctx, key, 0, -1) }

// GetRange reads n bytes at off (n < 0: to the end).
func (s *S3) GetRange(ctx context.Context, key string, off, n int64) ([]byte, error) {
	h := http.Header{}
	if off > 0 || n >= 0 {
		if n >= 0 {
			h.Set("Range", "bytes="+strconv.FormatInt(off, 10)+"-"+strconv.FormatInt(off+n-1, 10))
		} else {
			h.Set("Range", "bytes="+strconv.FormatInt(off, 10)+"-")
		}
	}
	resp, err := s.do(ctx, http.MethodGet, s.objectURL(key, nil), nil, h)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if resp.StatusCode >= 300 {
		return nil, s3Error("GET", key, resp)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if n >= 0 && int64(len(b)) != n {
		if resp.StatusCode == http.StatusOK && int64(len(b)) >= off+n {
			b = b[off : off+n] // server ignored Range
		} else {
			return nil, fmt.Errorf("tier: s3 GET %s: short range read (%d of %d bytes)", key, len(b), n)
		}
	}
	return b, nil
}

// List returns every object under prefix (ListObjectsV2, all pages).
func (s *S3) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}, "max-keys": {"1000"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		u := s.objectURL("", q)
		resp, err := s.do(ctx, http.MethodGet, u, nil, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 300 {
			err := s3Error("LIST", prefix, resp)
			resp.Body.Close()
			return nil, err
		}
		var r struct {
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
			Contents              []struct {
				Key  string `xml:"Key"`
				Size int64  `xml:"Size"`
			} `xml:"Contents"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&r)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("tier: s3 LIST %s: %w", prefix, err)
		}
		for _, c := range r.Contents {
			out = append(out, ObjectInfo{Key: c.Key, Size: c.Size})
		}
		if !r.IsTruncated || r.NextContinuationToken == "" {
			return out, nil
		}
		token = r.NextContinuationToken
	}
}

// Delete removes an object (a missing key is not an error).
func (s *S3) Delete(ctx context.Context, key string) error {
	resp, err := s.do(ctx, http.MethodDelete, s.objectURL(key, nil), nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return s3Error("DELETE", key, resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// EnsureBucket creates the bucket when a HEAD says it does not exist (for
// MinIO dev stacks; on AWS create buckets out of band).
func (s *S3) EnsureBucket(ctx context.Context) error {
	resp, err := s.do(ctx, http.MethodHead, s.objectURL("", nil), nil, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("tier: s3 HEAD bucket %s: HTTP %d", s.cfg.Bucket, resp.StatusCode)
	}
	var body []byte
	if s.cfg.Region != "us-east-1" {
		body = []byte("<CreateBucketConfiguration><LocationConstraint>" + s.cfg.Region + "</LocationConstraint></CreateBucketConfiguration>")
	}
	if body == nil {
		body = []byte{}
	}
	resp, err = s.do(ctx, http.MethodPut, s.objectURL("", nil), body, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return s3Error("CREATE BUCKET", s.cfg.Bucket, resp)
	}
	return nil
}
