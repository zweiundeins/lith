package backup

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// fakeS3 is gofakes3 in memory with the bucket "lith". gofakes3 answers a
// listing of multipart uploads in a bucket that never had one with
// NoSuchUpload, where S3 lists none: the handler answers as S3 does.
func fakeS3(t *testing.T) http.Handler {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket("lith"); err != nil {
		t.Fatal(err)
	}
	next := gofakes3.New(backend, gofakes3.WithLogger(gofakes3.DiscardLog())).Server()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !r.URL.Query().Has("uploads") {
			next.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		if rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), "NoSuchUpload") {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><ListMultipartUploadsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>lith</Bucket><IsTruncated>false</IsTruncated></ListMultipartUploadsResult>`)
			return
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
}

// stallingS3 is gofakes3 behind a handler that records the multipart uploads
// created and aborted, by key, and holds the parts of the stalled-th upload
// (counting from 1) until the client gives up. It knows nothing of the
// client's key layout.
type stallingS3 struct {
	stalled int
	entered chan struct{}
	once    sync.Once

	mu      sync.Mutex
	created []string
	aborted []string
}

func (s *stallingS3) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		s.mu.Lock()
		stall := len(s.created) >= s.stalled && r.URL.Path == s.created[s.stalled-1]
		s.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && q.Has("uploads"):
			s.mu.Lock()
			s.created = append(s.created, r.URL.Path)
			s.mu.Unlock()
		case r.Method == http.MethodPut && q.Has("partNumber") && stall:
			s.once.Do(func() { close(s.entered) })
			// The server notices the closed connection of a cancelled client
			// once the body has been read.
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			return
		case r.Method == http.MethodDelete && q.Has("uploadId"):
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, r)
			if rec.Code == http.StatusNoContent {
				s.mu.Lock()
				s.aborted = append(s.aborted, r.URL.Path)
				s.mu.Unlock()
			}
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes())
			return
		}
		next.ServeHTTP(w, r)
	})
}

// TestS3AbortOfACancelledSnapshot runs a snapshot of more than one part
// through litestream's S3 client against gofakes3, cuts it off with detach
// in the middle of its parts, and checks that the upload it leaves is found
// under the key layout that client writes and aborted before detach returns.
func TestS3AbortOfACancelledSnapshot(t *testing.T) {
	t.Parallel()
	// The first upload is the first sync's WAL segment, which Attach waits
	// for; the second is Attach's snapshot.
	stall := &stallingS3{stalled: 2, entered: make(chan struct{})}
	srv := httptest.NewServer(stall.wrap(fakeS3(t)))
	t.Cleanup(srv.Close)

	m, err := New(Config{
		Bucket: "lith", Endpoint: srv.URL, Region: "us-east-1", ForcePathStyle: true,
		AccessKeyID: "test", SecretAccessKey: "test", Prefix: "p",
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "app.db")
	db := openApp(t, m, path)
	mustExec(t, db, `CREATE TABLE blobs (b BLOB)`)
	for range 7 { // 7 MiB that do not compress: two parts of 5 MiB
		mustExec(t, db, `INSERT INTO blobs VALUES (randomblob(1 << 20))`)
	}
	detach, err := m.Attach(bg, "app", path)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-stall.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the snapshot's parts never arrived")
	}
	shutdown(t, db, detach)

	stall.mu.Lock()
	created, aborted := slices.Clone(stall.created), slices.Clone(stall.aborted)
	stall.mu.Unlock()
	if len(created) != 2 || !strings.HasPrefix(created[1], "/lith/p/app/") || !slices.Equal(aborted, created[1:]) {
		t.Errorf("uploads created %v, aborted %v; want the second aborted", created, aborted)
	}
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL), Region: "us-east-1", UsePathStyle: true,
		Credentials: aws.AnonymousCredentials{},
	})
	out, err := client.ListMultipartUploads(context.Background(), &s3.ListMultipartUploadsInput{Bucket: aws.String("lith")})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Uploads) != 0 {
		t.Errorf("unfinished uploads after detach: %d, first %s", len(out.Uploads), aws.ToString(out.Uploads[0].Key))
	}
}
