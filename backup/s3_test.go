package backup

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/benbjohnson/litestream"
)

// TestS3 runs the round trip, maintenance and purge against a bucket: a real
// one when LITH_BACKUP_TEST_BUCKET names it, with its credentials in
// LITH_BACKUP_TEST_ENDPOINT, _REGION, _ACCESS_KEY_ID, _SECRET_ACCESS_KEY and
// _FORCE_PATH_STYLE (MinIO: true), else gofakes3 in memory. Everything it
// writes lives under a fresh prefix and is purged at the end.
func TestS3(t *testing.T) {
	cfg, err := ConfigFromEnv("LITH_BACKUP_TEST_")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LITH_BACKUP_TEST_BUCKET") == "" {
		srv := httptest.NewServer(fakeS3(t))
		t.Cleanup(srv.Close)
		cfg = Config{Bucket: "lith", Endpoint: srv.URL, Region: "us-east-1", ForcePathStyle: true, AccessKeyID: "test", SecretAccessKey: "test"}
	}
	cfg.Prefix = fmt.Sprintf("lith-backup-test-%d", time.Now().UnixNano())
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "app.db")
	db, detach := replicated(t, m, "app", path)
	t.Cleanup(func() {
		if err := m.PurgeReplica(bg, "app"); err != nil {
			t.Errorf("PurgeReplica: %v", err)
		}
	})

	writeAndWaitLevel0(t, m, db, "app", 3)
	if errs := m.MaintainAll(bg, time.Now()); errs["app"] != nil {
		t.Fatalf("MaintainAll: %v", errs["app"])
	}
	if n := countLevel(t, m, "app", litestream.SnapshotLevel); n != 1 {
		t.Errorf("snapshots = %d, want 1", n)
	}
	mustExec(t, db, `INSERT INTO notes (body) VALUES ('just before shutdown')`)
	shutdown(t, db, detach)

	if err := m.VerifyRestore(bg, "app"); err != nil {
		t.Fatalf("VerifyRestore: %v", err)
	}
	removeDB(t, path)
	if err := m.RestoreIfMissing(bg, "app", path); err != nil {
		t.Fatalf("RestoreIfMissing: %v", err)
	}
	if got := bodies(t, path); len(got) != 5 {
		t.Errorf("restored rows = %v, want 5", got)
	}
}
