package backup

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// regionS3 answers HEAD /<bucket> the way S3 tells a bucket's region: in
// X-Amz-Bucket-Region, with 200 in the caller's region, 301 in another and
// 403 for a bucket the caller may not see; 404 without it for no bucket.
// It counts the signed requests.
func regionS3(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var signed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			signed.Add(1)
		}
		answers := map[string]struct {
			status int
			region string
		}{
			"here":    {http.StatusOK, "us-east-1"},
			"moved":   {http.StatusMovedPermanently, "eu-central-2"},
			"refused": {http.StatusForbidden, "ap-southeast-2"},
		}
		a, ok := answers[strings.Trim(r.URL.Path, "/")]
		if r.Method != http.MethodHead || !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("X-Amz-Bucket-Region", a.region)
		w.WriteHeader(a.status)
	}))
	t.Cleanup(srv.Close)
	return srv, &signed
}

func TestBucketRegion(t *testing.T) {
	srv, signed := regionS3(t)
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
	})
	for bucket, want := range map[string]string{"here": "us-east-1", "moved": "eu-central-2", "refused": "ap-southeast-2"} {
		if got, err := bucketRegion(bg, client, bucket); err != nil || got != want {
			t.Errorf("bucketRegion(%s) = %q, %v; want %q", bucket, got, err, want)
		}
	}
	if got, err := bucketRegion(bg, client, "missing"); err == nil {
		t.Errorf("bucketRegion(missing) = %q, want an error", got)
	}
	if n := signed.Load(); n != 0 {
		t.Errorf("%d lookups were signed, want none", n)
	}
}

// TestARegionlessBucketFindsItsRegion: a Config with a bucket and keys but
// no region or endpoint, on a host that names no region either.
func TestARegionlessBucketFindsItsRegion(t *testing.T) {
	srv, _ := regionS3(t)
	dir := t.TempDir()
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_ENDPOINT_URL_S3", srv.URL) // stands in for AWS, since the Config names no endpoint
	m := &Manager{cfg: Config{Bucket: "moved", ForcePathStyle: true, AccessKeyID: "test", SecretAccessKey: "test"}}
	api, err := m.newMultipartAPI(bg)
	if err != nil {
		t.Fatal(err)
	}
	if got := api.(*s3.Client).Options().Region; got != "eu-central-2" {
		t.Errorf("region = %q, want eu-central-2", got)
	}
}
