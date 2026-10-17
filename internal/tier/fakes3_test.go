package tier

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/himanshubaliyan7/zerowatch/internal/awssig"
)

// fakeS3 is an in-process path-style S3: PUT, GET (with Range), LIST v2
// (paged by pageSize), DELETE. It insists on a SigV4 Authorization header
// scoped to s3 and a payload hash header.
type fakeS3 struct {
	mu       sync.Mutex
	bucket   string
	objs     map[string][]byte
	pageSize int
	gets     int
	ranged   int
	fail     bool // answer 500 to everything
}

func newFakeS3(t *testing.T) (*fakeS3, *S3) {
	f := &fakeS3{bucket: "zw", objs: map[string][]byte{}, pageSize: 3}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := NewS3(S3Config{Endpoint: srv.URL, Bucket: "zw", PathStyle: true, Creds: awssig.Credentials{AccessKeyID: "AK", Secret: "SK"}, Client: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func (f *fakeS3) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.objs))
	for k := range f.objs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		http.Error(w, "<Error><Code>InternalError</Code><Message>down</Message></Error>", 500)
		return
	}
	if !strings.Contains(r.Header.Get("Authorization"), "/s3/aws4_request") || r.Header.Get("X-Amz-Content-Sha256") == "" {
		http.Error(w, "<Error><Code>AccessDenied</Code></Error>", 403)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/"+f.bucket)
	key := strings.TrimPrefix(path, "/")
	switch {
	case r.Method == http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		f.objs[key] = b
	case r.Method == http.MethodDelete:
		delete(f.objs, key)
		w.WriteHeader(204)
	case r.Method == http.MethodGet && key == "" && r.URL.Query().Get("list-type") == "2":
		prefix := r.URL.Query().Get("prefix")
		var ks []string
		for k := range f.objs {
			if strings.HasPrefix(k, prefix) {
				ks = append(ks, k)
			}
		}
		sort.Strings(ks)
		start := 0
		if tok := r.URL.Query().Get("continuation-token"); tok != "" {
			start, _ = strconv.Atoi(tok)
		}
		type content struct {
			Key  string
			Size int64
		}
		res := struct {
			XMLName               xml.Name `xml:"ListBucketResult"`
			IsTruncated           bool
			NextContinuationToken string `xml:",omitempty"`
			Contents              []content
		}{}
		end := min(start+f.pageSize, len(ks))
		for _, k := range ks[start:end] {
			res.Contents = append(res.Contents, content{k, int64(len(f.objs[k]))})
		}
		if end < len(ks) {
			res.IsTruncated, res.NextContinuationToken = true, strconv.Itoa(end)
		}
		_ = xml.NewEncoder(w).Encode(res)
	case r.Method == http.MethodGet:
		b, ok := f.objs[key]
		if !ok {
			http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", 404)
			return
		}
		f.gets++
		if rg := r.Header.Get("Range"); rg != "" {
			f.ranged++
			lo, hi, _ := strings.Cut(strings.TrimPrefix(rg, "bytes="), "-")
			a, _ := strconv.Atoi(lo)
			z := len(b) - 1
			if hi != "" {
				z, _ = strconv.Atoi(hi)
			}
			w.WriteHeader(206)
			_, _ = w.Write(b[a : z+1])
			return
		}
		_, _ = w.Write(b)
	default:
		http.Error(w, "bad", 400)
	}
}
