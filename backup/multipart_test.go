package backup

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/benbjohnson/litestream"
)

// fakeMultipart keeps unfinished uploads by key and lists them two a page.
type fakeMultipart struct {
	mu        sync.Mutex
	keys      []string // sorted
	listed    []string // the prefix of each listing
	failAbort error
}

func (f *fakeMultipart) ListMultipartUploads(_ context.Context, in *s3.ListMultipartUploadsInput, _ ...func(*s3.Options)) (*s3.ListMultipartUploadsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed = append(f.listed, aws.ToString(in.Prefix))
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
// its own name.
func TestPurgeAbortsUnfinishedUploads(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
	shutdown(t, db, detach)
	var keys []string
	for i := range 5 {
		keys = append(keys, fmt.Sprintf("app/ltx/9/%016x-%016x.ltx", 1, i+1))
	}
	f := withMultipart(m, append(keys, "app2/ltx/9/0000000000000001-0000000000000001.ltx")...)

	if err := m.PurgeReplica(bg, "app"); err != nil {
		t.Fatal(err)
	}
	left, listed := f.state()
	if !slices.Equal(left, []string{"app2/ltx/9/0000000000000001-0000000000000001.ltx"}) {
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
	f := withMultipart(m, "app/ltx/9/0000000000000001-0000000000000002.ltx")
	f.failAbort = errors.New("403 AccessDenied")

	if err := m.PurgeReplica(bg, "app"); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("PurgeReplica = %v, want the abort's error", err)
	}
	if n := countLevel(t, m, "app", 0); n == 0 {
		t.Error("the purge deleted the replica although it could not abort its uploads")
	}
}

// TestACancelledSnapshotAbortsItsUploads: a snapshot that detach cuts off
// leaves its parts, so they are aborted, at the snapshot level only.
func TestACancelledSnapshotAbortsItsUploads(t *testing.T) {
	t.Parallel()
	m, h := hookedManager(t)
	f := withMultipart(m, "app/ltx/9/0000000000000001-0000000000000002.ltx", "app/ltx/0/0000000000000003-0000000000000003.ltx")
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
	waitFor(t, 5*time.Second, func() bool {
		left, _ := f.state()
		return len(left) == 1
	})
	left, listed := f.state()
	if want := fmt.Sprintf("app/ltx/%d/", litestream.SnapshotLevel); !slices.Equal(listed, []string{want}) || !strings.Contains(left[0], "/ltx/0/") {
		t.Errorf("listed %v and left %v, want the snapshot level's upload aborted under %s", listed, left, want)
	}
}
