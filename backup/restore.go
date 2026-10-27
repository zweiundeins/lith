package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/benbjohnson/litestream"
	"github.com/superfly/ltx"
)

// A restore reads the latest snapshot and every WAL segment since, which on a
// replica under constant writes are thousands of small files. Litestream's
// Replica.Restore opens all of them at once, one connection each, and holds
// them until the end. This restore downloads them to disk with
// restoreFetchers connections and merges them restoreFanIn at a time, so
// what a restore holds does not grow with the replica.
const (
	restoreFetchers = 4
	restoreFanIn    = 32

	restoreFetchAttempts = 3
	restoreFetchBackoff  = 250 * time.Millisecond
)

// restoreStats describes a restore that wrote its output.
type restoreStats struct {
	files int   // LTX files in the plan
	bytes int64 // the restored database
}

// restoreTo writes the replica client reads as of at (zero for the latest) to
// out, which must not exist, and checks it with check. Its working files go
// in a directory beside out, removed at the end.
func (m *Manager) restoreTo(ctx context.Context, client litestream.ReplicaClient, out string, at time.Time, check litestream.IntegrityCheckMode) (restoreStats, error) {
	var st restoreStats
	if _, err := os.Stat(out); err == nil {
		return st, fmt.Errorf("cannot restore, output path already exists: %s", out)
	} else if !errors.Is(err, os.ErrNotExist) {
		return st, err
	}
	plan, err := litestream.CalcRestorePlan(ctx, client, 0, at, m.log)
	if errors.Is(err, litestream.ErrNoSnapshots) || errors.Is(err, litestream.ErrTxNotAvailable) {
		return st, ErrNothingToRestore
	} else if err != nil {
		return st, fmt.Errorf("cannot calc restore plan: %w", err)
	}
	st.files = len(plan)
	for _, info := range plan {
		if info.Size < ltx.HeaderSize {
			return st, fmt.Errorf("invalid ltx file: level=%d min=%s max=%s has size %d bytes (minimum %d)",
				info.Level, info.MinTXID, info.MaxTXID, info.Size, ltx.HeaderSize)
		}
	}
	dir := filepath.Dir(out)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return st, fmt.Errorf("create parent directory: %w", err)
	}
	spool, err := os.MkdirTemp(dir, "."+filepath.Base(out)+"-restore-")
	if err != nil {
		return st, err
	}
	defer func() { _ = os.RemoveAll(spool) }()
	r := &restorer{client: client, spool: spool}

	// The first file, the snapshot, stays as it is: the WAL segments after it
	// are merged into fewer files until they and it fit in one merge.
	first, err := r.fetch(ctx, plan[:1])
	if err != nil {
		return st, err
	}
	var rest []string
	for i := 1; i < len(plan); i += restoreFanIn {
		group, err := r.fetch(ctx, plan[i:min(i+restoreFanIn, len(plan))])
		if err != nil {
			return st, err
		}
		merged, err := r.merge(ctx, group)
		if err != nil {
			return st, err
		}
		rest = append(rest, merged)
	}
	for len(rest) > restoreFanIn-1 {
		var next []string
		for i := 0; i < len(rest); i += restoreFanIn {
			merged, err := r.merge(ctx, rest[i:min(i+restoreFanIn, len(rest))])
			if err != nil {
				return st, err
			}
			next = append(next, merged)
		}
		rest = next
	}

	tmp := out + ".tmp"
	defer func() { _ = os.Remove(tmp) }()
	if err := r.decode(ctx, append(first, rest...), tmp); err != nil {
		return st, err
	}
	if err := os.Rename(tmp, out); err != nil {
		return st, err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	if err := integrityCheck(ctx, out, check); err != nil {
		if ctx.Err() == nil {
			for _, suffix := range []string{"", "-wal", "-shm"} {
				_ = os.Remove(out + suffix)
			}
		}
		return st, fmt.Errorf("post-restore integrity check: %w", err)
	}
	if fi, err := os.Stat(out); err == nil {
		st.bytes = fi.Size()
	}
	return st, nil
}

// restorer holds the working files of one restore.
type restorer struct {
	client litestream.ReplicaClient
	spool  string
	n      int // files named so far
	mu     sync.Mutex
}

func (r *restorer) name() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	return filepath.Join(r.spool, fmt.Sprintf("%08d.ltx", r.n))
}

// fetch downloads the files in infos, restoreFetchers at a time, and
// returns their local paths in the same order.
func (r *restorer) fetch(ctx context.Context, infos []*ltx.FileInfo) ([]string, error) {
	paths := make([]string, len(infos))
	for i := range infos {
		paths[i] = r.name()
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	sem := make(chan struct{}, restoreFetchers)
	var wg sync.WaitGroup
	for i, info := range infos {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := r.fetchOne(ctx, info, paths[i]); err != nil {
				cancel(err)
			}
		})
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	return paths, nil
}

// fetchOne downloads one file. The S3 client retries a request that fails;
// this retries a body cut off in the middle.
func (r *restorer) fetchOne(ctx context.Context, info *ltx.FileInfo, path string) error {
	var err error
	for attempt := range restoreFetchAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(restoreFetchBackoff << (attempt - 1)):
			}
		}
		var rc io.ReadCloser
		rc, err = r.client.OpenLTXFile(ctx, info.Level, info.MinTXID, info.MaxTXID, 0, 0)
		if err != nil {
			return fmt.Errorf("open ltx file %s: %w", ltx.FormatFilename(info.MinTXID, info.MaxTXID), err)
		}
		err = writeFile(path, rc, info.Size)
		_ = rc.Close()
		if err == nil || ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("read ltx file %s: %w", ltx.FormatFilename(info.MinTXID, info.MaxTXID), err)
	}
	return nil
}

func writeFile(path string, r io.Reader, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && size > 0 && n != size {
		err = fmt.Errorf("got %d bytes, want %d", n, size)
	}
	return err
}

// merge compacts the files at paths into one and removes them.
func (r *restorer) merge(ctx context.Context, paths []string) (string, error) {
	if len(paths) == 1 {
		return paths[0], nil
	}
	out := r.name()
	f, err := os.Create(out)
	if err != nil {
		return "", err
	}
	err = compact(ctx, f, paths, r.spool)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("merge ltx files: %w", err)
	}
	for _, p := range paths {
		_ = os.Remove(p)
	}
	return out, nil
}

// decode compacts the files at paths, a snapshot first, into the database
// file out.
func (r *restorer) decode(ctx context.Context, paths []string, out string) error {
	f, err := os.Create(out)
	if err != nil {
		return fmt.Errorf("create temp database path: %w", err)
	}
	defer func() { _ = f.Close() }()
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = pw.CloseWithError(compact(ctx, pw, paths, r.spool))
	}()
	err = ltx.NewDecoder(pr).DecodeDatabaseTo(f)
	// The compaction stops at its next write once the reading end is closed.
	_ = pr.CloseWithError(errors.Join(err, errors.New("restore: decode stopped")))
	<-done
	if err != nil {
		return fmt.Errorf("decode database: %w", err)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

// compact merges the LTX files at paths, in TXID order, into w.
func compact(ctx context.Context, w io.Writer, paths []string, spill string) error {
	rdrs := make([]io.Reader, 0, len(paths))
	defer func() {
		for _, rd := range rdrs {
			_ = rd.(*os.File).Close()
		}
	}()
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		rdrs = append(rdrs, f)
	}
	c, err := ltx.NewCompactor(w, rdrs)
	if err != nil {
		return err
	}
	c.HeaderFlags = ltx.HeaderFlagNoChecksum
	c.SetSpillDir(spill)
	return c.Compact(ctx)
}

// integrityCheck runs quick_check or integrity_check on the database at path,
// as litestream's restore does.
func integrityCheck(ctx context.Context, path string, mode litestream.IntegrityCheckMode) error {
	var pragma string
	switch mode {
	case litestream.IntegrityCheckNone:
		return nil
	case litestream.IntegrityCheckQuick:
		pragma = "quick_check"
	case litestream.IntegrityCheckFull:
		pragma = "integrity_check"
	default:
		return fmt.Errorf("unsupported integrity check mode: %d", mode)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("integrity check failed: %s", result)
	}
	_ = db.Close()
	_ = os.Remove(path + "-shm")
	_ = os.Remove(path + "-wal")
	return nil
}
