package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/benbjohnson/litestream"
)

// TestS3 runs the round trip, maintenance and purge against a real bucket.
// It needs an existing bucket in LITH_BACKUP_TEST_BUCKET and its credentials
// in LITH_BACKUP_TEST_ENDPOINT, _REGION, _ACCESS_KEY_ID, _SECRET_ACCESS_KEY
// and _FORCE_PATH_STYLE (MinIO: true). Everything it writes lives under a
// fresh prefix and is purged at the end.
func TestS3(t *testing.T) {
	if os.Getenv("LITH_BACKUP_TEST_BUCKET") == "" {
		t.Skip("needs an S3 bucket: set LITH_BACKUP_TEST_BUCKET and its credentials (see the test's comment)")
	}
	cfg, err := ConfigFromEnv("LITH_BACKUP_TEST_")
	if err != nil {
		t.Fatal(err)
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
