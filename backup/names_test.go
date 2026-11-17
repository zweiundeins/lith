package backup

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
)

// pagedS3 is gofakes3 behind a handler that asks every object listing for
// two entries a page, and counts the listings.
type pagedS3 struct{ listings atomic.Int32 }

func (p *pagedS3) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); r.Method == http.MethodGet && q.Get("list-type") == "2" {
			p.listings.Add(1)
			q.Set("max-keys", "2")
			r.URL.RawQuery = q.Encode()
		}
		next.ServeHTTP(w, r)
	})
}

func TestReplicaNamesS3(t *testing.T) {
	t.Parallel()
	p := &pagedS3{}
	srv := httptest.NewServer(p.wrap(fakeS3(t)))
	t.Cleanup(srv.Close)
	putKeys(t, srv.URL,
		"p/e/0000/x.ltx", "p/a/0000/x.ltx", "p/a/0001/y.ltx", "p/c/x", "p/d/0000/x.ltx", "p/b/0000/x.ltx",
		"p/stray", "other/z/0000/x.ltx")
	names := func(prefix string) ([]string, int32) {
		t.Helper()
		m, err := New(Config{Bucket: "lith", Endpoint: srv.URL, Region: "us-east-1", ForcePathStyle: true, AccessKeyID: "test", SecretAccessKey: "test", Prefix: prefix})
		if err != nil {
			t.Fatal(err)
		}
		before := p.listings.Load()
		got, err := m.ReplicaNames(bg)
		if err != nil {
			t.Fatal(err)
		}
		return got, p.listings.Load() - before
	}
	if got, pages := names("p"); !slices.Equal(got, []string{"a", "b", "c", "d", "e"}) || pages < 3 {
		t.Errorf("ReplicaNames under p = %v in %d pages, want a to e in 3 or more", got, pages)
	}
	if got, _ := names(""); !slices.Equal(got, []string{"other", "p"}) {
		t.Errorf("ReplicaNames without a prefix = %v, want other and p", got)
	}
	if got, _ := names("none"); got != nil {
		t.Errorf("ReplicaNames under an empty prefix = %v, want none", got)
	}
}

func TestReplicaNamesDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m, err := New(Config{Dir: dir, Prefix: "p/q"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := m.ReplicaNames(bg); got != nil || err != nil {
		t.Fatalf("ReplicaNames before any replica = %v, %v", got, err)
	}
	db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
	shutdown(t, db, detach)
	for _, d := range []string{"p/q/gone", "p/q/b", "p/other"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "p/q/stray"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := m.ReplicaNames(bg); err != nil || !slices.Equal(got, []string{"app", "b", "gone"}) {
		t.Errorf("ReplicaNames = %v, %v; want app, b and gone", got, err)
	}
}

func TestReplicaNamesDisabled(t *testing.T) {
	m, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := m.ReplicaNames(bg); got != nil || err != nil {
		t.Errorf("ReplicaNames = %v, %v; want nil, nil", got, err)
	}
}
