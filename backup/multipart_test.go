package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/benbjohnson/litestream"
)

// fakeMultipart keeps unfinished uploads by key and lists them two a page.
type fakeMultipart struct {
	mu        sync.Mutex
	keys      []string // sorted
	listed    []string // the prefix of each listing
	failAbort error
	failList  error
	stuck     bool // every page says it is truncated, with the same markers
}

func (f *fakeMultipart) ListMultipartUploads(_ context.Context, in *s3.ListMultipartUploadsInput, _ ...func(*s3.Options)) (*s3.ListMultipartUploadsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed = append(f.listed, aws.ToString(in.Prefix))
	if f.failList != nil {
		return nil, f.failList
	}
	if f.stuck {
		return &s3.ListMultipartUploadsOutput{IsTruncated: aws.Bool(true), NextKeyMarker: aws.String("k"), NextUploadIdMarker: aws.String("u")}, nil
	}
	out := &s3.ListMultipartUploadsOutput{}
	for _, k := range f.keys {
		if !strings.HasPrefix(k, aws.ToString(in.Prefix)) || k <= aws.ToString(in.KeyMarker) {
			continue
		}
		if len(out.Uploads) == 2 {
			out.IsTruncated = aws.Bool(true)
			out.NextKeyMarker = out.Uploads[1].Key
			out.NextUploadIdMarker = out.Uploads[1].UploadId
			break
		}
		out.Uploads = append(out.Uploads, types.MultipartUpload{Key: aws.String(k), UploadId: aws.String("id-" + k)})
	}
	return out, nil
}

func (f *fakeMultipart) AbortMultipartUpload(_ context.Context, in *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAbort != nil {
		return nil, f.failAbort
	}
	if aws.ToString(in.UploadId) != "id-"+aws.ToString(in.Key) {
		return nil, &types.NoSuchUpload{}
	}
	f.keys = slices.DeleteFunc(f.keys, func(k string) bool { return k == aws.ToString(in.Key) })
	return &s3.AbortMultipartUploadOutput{}, nil
}

func (f *fakeMultipart) state() (keys, listed []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.keys), slices.Clone(f.listed)
}

func withMultipart(m *Manager, keys ...string) *fakeMultipart {
	f := &fakeMultipart{keys: slices.Sorted(slices.Values(keys))}
	m.mpu = &multipartClient{newAPI: func(context.Context) (multipartAPI, error) { return f, nil }}
	return f
}

// TestPurgeAbortsUnfinishedUploads: the parts of an upload cut off are not
// objects, so the purge aborts them too, every page of them, and only under
// its own name. Keys are in the S3 client's layout.
func TestPurgeAbortsUnfinishedUploads(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
	shutdown(t, db, detach)
	var keys []string
	for i := range 5 {
		keys = append(keys, fmt.Sprintf("app/0009/%016x-%016x.ltx", 1, i+1))
	}
	f := withMultipart(m, append(keys, "app2/0009/0000000000000001-0000000000000001.ltx")...)

	if err := m.PurgeReplica(bg, "app"); err != nil {
		t.Fatal(err)
	}
	left, listed := f.state()
	if !slices.Equal(left, []string{"app2/0009/0000000000000001-0000000000000001.ltx"}) {
		t.Errorf("uploads left = %v, want app2's alone", left)
	}
	if len(listed) != 3 || listed[0] != "app/" {
		t.Errorf("listings = %v, want three pages under app/", listed)
	}
	if n := countLevel(t, m, "app", 0); n != 0 {
		t.Errorf("WAL segments = %d after the purge, want none", n)
	}
}

// TestAPurgeThatCannotAbortFails: it deletes nothing, so a retry finds the
// uploads again.
func TestAPurgeThatCannotAbortFails(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
	shutdown(t, db, detach)
	f := withMultipart(m, "app/0009/0000000000000001-0000000000000002.ltx")
	f.failAbort = errors.New("403 AccessDenied")

	if err := m.PurgeReplica(bg, "app"); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("PurgeReplica = %v, want the abort's error", err)
	}
	if n := countLevel(t, m, "app", 0); n == 0 {
		t.Error("the purge deleted the replica although it could not abort its uploads")
	}
}

// TestAListingThatDoesNotMoveOnEnds: a provider that says every page is
// truncated without moving its markers fails the purge after one more page.
func TestAListingThatDoesNotMoveOnEnds(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	f := withMultipart(m)
	f.stuck = true
	if err := m.PurgeReplica(bg, "app"); err == nil || !strings.Contains(err.Error(), "does not move on") {
		t.Fatalf("PurgeReplica = %v, want the stuck listing", err)
	}
	if _, listed := f.state(); len(listed) != 2 {
		t.Errorf("listings = %d, want 2", len(listed))
	}
}

// TestAProviderWithoutMultipartListingsCanPurge: NotImplemented means there
// is nothing the Manager can abort, so the purge goes on, and says so once.
func TestAProviderWithoutMultipartListingsCanPurge(t *testing.T) {
	t.Parallel()
	msgs := &messages{prefix: "backup: the provider does not list"}
	m, err := New(Config{Dir: t.TempDir(), Logger: slog.New(msgs)})
	if err != nil {
		t.Fatal(err)
	}
	db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
	shutdown(t, db, detach)
	f := withMultipart(m)
	f.failList = fmt.Errorf("operation error S3: ListMultipartUploads: %w", &smithy.GenericAPIError{Code: "NotImplemented", Message: "A header you provided implies functionality that is not implemented"})

	for range 2 {
		if err := m.PurgeReplica(bg, "app"); err != nil {
			t.Fatalf("PurgeReplica = %v, want the purge done", err)
		}
	}
	if n := countLevel(t, m, "app", 0); n != 0 {
		t.Errorf("WAL segments = %d after the purge, want none", n)
	}
	if got := msgs.levels(); len(got) != 1 {
		t.Errorf("logged %v, want one line", got)
	}
}

// TestAPurgeHoldsTheName: an Attach while the purge talks to the bucket is
// refused, not let in between the abort and the delete.
func TestAPurgeHoldsTheName(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	path := filepath.Join(t.TempDir(), "app.db")
	db, detach := replicated(t, m, "app", path)
	if err := detach(bg); err != nil {
		t.Fatal(err)
	}
	defer closeApp(t, db)
	entered, release := make(chan struct{}), make(chan struct{})
	m.mpu = &multipartClient{newAPI: func(context.Context) (multipartAPI, error) {
		close(entered)
		<-release
		return nil, nil
	}}
	purged := make(chan error, 1)
	go func() { purged <- m.PurgeReplica(bg, "app") }()
	<-entered
	if _, err := m.Attach(bg, "app", path); err == nil || !strings.Contains(err.Error(), "purged") {
		t.Errorf("Attach during the purge = %v, want it refused", err)
	}
	if err := m.PurgeReplica(bg, "app"); err == nil {
		t.Error("a second purge of the name ran beside the first")
	}
	close(release)
	if err := <-purged; err != nil {
		t.Fatal(err)
	}
	detach, err := m.Attach(bg, "app", path)
	if err != nil {
		t.Fatalf("Attach after the purge: %v", err)
	}
	if err := detach(bg); err != nil {
		t.Fatal(err)
	}
}

// TestACancelledSnapshotAbortsItsUploads: a snapshot that detach cuts off
// leaves its parts, so they are aborted, at the snapshot level only, before
// detach returns.
func TestACancelledSnapshotAbortsItsUploads(t *testing.T) {
	t.Parallel()
	m, h := hookedManager(t)
	f := withMultipart(m, "app/0009/0000000000000001-0000000000000002.ltx", "app/0000/0000000000000003-0000000000000003.ltx")
	entered := make(chan struct{})
	snapshot := func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	h.snapshot.Store(&snapshot)

	path := filepath.Join(t.TempDir(), "app.db")
	db := openApp(t, m, path)
	mustExec(t, db, `CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)`)
	detach, err := m.Attach(bg, "app", path)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	shutdown(t, db, detach)
	left, listed := f.state()
	if want := s3KeyPrefix("app", litestream.SnapshotLevel); !slices.Equal(listed, []string{want}) || len(left) != 1 || !strings.Contains(left[0], "/0000/") {
		t.Errorf("listed %v and left %v, want the snapshot level's upload aborted under %s", listed, left, want)
	}
}

// TestATimedOutSnapshotAbortsItsUploads: SnapshotTimeout ends the snapshot
// without a detach, and its upload is aborted all the same.
func TestATimedOutSnapshotAbortsItsUploads(t *testing.T) {
	t.Parallel()
	m, h := hookedManager(t)
	m.snapshotTimeout = 50 * time.Millisecond
	f := withMultipart(m, "app/0009/0000000000000001-0000000000000002.ltx")
	snapshot := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	h.snapshot.Store(&snapshot)

	path := filepath.Join(t.TempDir(), "app.db")
	db := openApp(t, m, path)
	mustExec(t, db, `CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)`)
	detach, err := m.Attach(bg, "app", path)
	if err != nil {
		t.Fatal(err)
	}
	waitForAttachSnapshot(m, "app")
	if left, _ := f.state(); len(left) != 0 {
		t.Errorf("uploads left = %v after the timeout, want none", left)
	}
	shutdown(t, db, detach)
}
