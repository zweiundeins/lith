package backup

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// headerS3 is gofakes3 behind a handler that records, per request kind,
// the Content-MD5 each request carried and whether it matched the body.
type headerS3 struct {
	mu   sync.Mutex
	puts []string // PutObject, not parts
	dels []string // DeleteObjects
	bad  []string // Content-MD5 that does not match the body
}

func (h *headerS3) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		put := r.Method == http.MethodPut && !q.Has("partNumber") && !q.Has("uploadId")
		del := r.Method == http.MethodPost && q.Has("delete")
		if put || del {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			sum := md5.Sum(body)
			md := r.Header.Get("Content-MD5")
			h.mu.Lock()
			if put {
				h.puts = append(h.puts, md)
			} else {
				h.dels = append(h.dels, md)
			}
			if md != "" && md != base64.StdEncoding.EncodeToString(sum[:]) {
				h.bad = append(h.bad, r.URL.Path)
			}
			h.mu.Unlock()
		}
		next.ServeHTTP(w, r)
	})
}

// Litestream's PutObject uploads carry Content-MD5 with ContentMD5 set, and
// none without.
func TestS3ContentMD5(t *testing.T) {
	t.Parallel()
	for _, on := range []bool{true, false} {
		h := &headerS3{}
		srv := httptest.NewServer(h.wrap(fakeS3(t)))
		t.Cleanup(srv.Close)
		m, err := New(Config{
			Bucket: "lith", Endpoint: srv.URL, Region: "us-east-1", ForcePathStyle: true,
			AccessKeyID: "test", SecretAccessKey: "test", ContentMD5: on,
			Logger: slog.New(slog.DiscardHandler),
		})
		if err != nil {
			t.Fatal(err)
		}
		db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
		shutdown(t, db, detach)

		h.mu.Lock()
		puts, bad := h.puts, h.bad
		h.mu.Unlock()
		if len(puts) == 0 {
			t.Fatalf("ContentMD5 %v: no PutObject", on)
		}
		for _, md := range puts {
			if (md != "") != on {
				t.Errorf("ContentMD5 %v: PutObject with Content-MD5 %q", on, md)
			}
		}
		if len(bad) > 0 {
			t.Errorf("ContentMD5 %v: Content-MD5 does not match the body of %v", on, bad)
		}
	}
}

// lith's own S3 client takes litestream's checksum policy: with an
// Endpoint, checksums only where an operation requires one.
func TestS3ClientChecksumPolicy(t *testing.T) {
	for _, c := range []struct {
		endpoint string
		req      aws.RequestChecksumCalculation
		resp     aws.ResponseChecksumValidation
	}{
		{"http://127.0.0.1:1", aws.RequestChecksumCalculationWhenRequired, aws.ResponseChecksumValidationWhenRequired},
		{"", aws.RequestChecksumCalculationWhenSupported, aws.ResponseChecksumValidationWhenSupported},
	} {
		m, err := New(Config{Bucket: "lith", Endpoint: c.endpoint, Region: "us-east-1", AccessKeyID: "test", SecretAccessKey: "test"})
		if err != nil {
			t.Fatal(err)
		}
		api, err := m.newMultipartAPI(bg)
		if err != nil {
			t.Fatal(err)
		}
		o := api.(*s3.Client).Options()
		if o.RequestChecksumCalculation != c.req || o.ResponseChecksumValidation != c.resp {
			t.Errorf("Endpoint %q: request checksums %v, response validation %v; want %v, %v",
				c.endpoint, o.RequestChecksumCalculation, o.ResponseChecksumValidation, c.req, c.resp)
		}
	}
}

// With the Endpoint policy, lith's listings and DeleteObjects still pass
// through gofakes3, and DeleteObjects carries a matching Content-MD5.
func TestS3DeleteObjectsContentMD5(t *testing.T) {
	t.Parallel()
	h := &headerS3{}
	srv := httptest.NewServer(h.wrap(fakeS3(t)))
	t.Cleanup(srv.Close)
	m, err := New(Config{
		Bucket: "lith", Endpoint: srv.URL, Region: "us-east-1", ForcePathStyle: true,
		AccessKeyID: "test", SecretAccessKey: "test", ContentMD5: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	putKeys(t, srv.URL, "app/0000/a.ltx", "app/0000/b.ltx", "app/0001/c.ltx")
	api, err := m.multipart(bg)
	if err != nil {
		t.Fatal(err)
	}
	objs := api.(objectsAPI)
	keys, err := listKeys(bg, objs, aws.String("lith"), "app/")
	if err != nil || len(keys) != 3 {
		t.Fatalf("listKeys = %v, %v; want 3 keys", keys, err)
	}
	if err := deleteKeys(bg, objs, aws.String("lith"), keys); err != nil {
		t.Fatal(err)
	}
	if keys, err := listKeys(bg, objs, aws.String("lith"), "app/"); err != nil || len(keys) != 0 {
		t.Fatalf("after deleteKeys: %v, %v", keys, err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.dels) != 1 || h.dels[0] == "" || len(h.bad) > 0 {
		t.Errorf("DeleteObjects Content-MD5 %q, mismatched %v; want one that matches", h.dels, h.bad)
	}
}

// putKeys writes an object of one byte at each key in the bucket "lith".
func putKeys(t *testing.T, endpoint string, keys ...string) {
	t.Helper()
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(endpoint), Region: "us-east-1", UsePathStyle: true,
		Credentials: aws.AnonymousCredentials{},
	})
	for _, k := range keys {
		if _, err := client.PutObject(bg, &s3.PutObjectInput{Bucket: aws.String("lith"), Key: aws.String(k), Body: bytes.NewReader([]byte{1})}); err != nil {
			t.Fatal(err)
		}
	}
}
