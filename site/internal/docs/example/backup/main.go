// Command backup starts and stops an app whose database Litestream
// replicates to S3, with lith's backup module. It is a module of its own,
// as the backup module is: the site's module never pulls in Litestream.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/zweiundeins/lith/backup"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"
)

func main() {
	if err := run("data/app.db"); err != nil {
		slog.Error("app", "err", err)
		os.Exit(1)
	}
}

func run(path string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// BACKUP_BUCKET, BACKUP_REGION, BACKUP_ACCESS_KEY_ID and the rest. Without
	// a bucket the manager is disabled and every call below does nothing,
	// so development runs without credentials.
	cfg, err := backup.ConfigFromEnv("BACKUP_")
	if err != nil {
		return err
	}
	m, err := backup.New(cfg)
	if err != nil {
		return err
	}

	// 1. Before the database opens: a fresh disk gets the replica's state.
	if err := m.RestoreIfMissing(ctx, "app", path); err != nil {
		return err
	}
	// 2. Litestream checkpoints at positions it has replicated. Without it
	// the writer does, or the WAL would grow without end.
	checkpoint := sqlite.CheckpointWriter
	if m.Enabled() {
		checkpoint = sqlite.CheckpointExternal
	}
	db, err := sqlite.Open(ctx, path, sqlite.Options{Checkpoint: checkpoint})
	if err != nil {
		return err
	}
	w, err := writer.New(db, writer.Options{})
	if err != nil {
		return errors.Join(err, db.Close())
	}
	go w.Run(context.Background())
	// 3. Replicate. Attach returns once the first sync reached the replica.
	detach, err := m.Attach(ctx, "app", path)
	if err != nil {
		return errors.Join(err, w.Close(), db.Close())
	}

	jobs, stopJobs := context.WithCancel(context.Background())
	defer stopJobs()
	go m.RunMaintenanceLoop(jobs, 0) // snapshots daily, history pruned after 30 days
	// Verifies daily, and retries a failure after 10 and 30 minutes.
	go m.RunVerifyLoop(jobs, backup.VerifyOptions{
		// An app that attaches its tenants on demand sets Names to its account list.
		OnFail: func(failures []backup.VerifyFailure, span time.Duration) {
			// Once per run, with the replicas that failed every attempt: alert here.
			var names []string
			for _, f := range failures {
				names = append(names, f.Name)
			}
			slog.Error("backup: replicas do not restore", "dbs", names, "first", failures[0].First, "last", failures[0].Last, "span", span)
		},
	})
	prometheus.MustRegister(m.Collector("app")) // app_backup_enabled 0 is the alert for no backup

	srv := &http.Server{Addr: "127.0.0.1:8080", Handler: http.NewServeMux()}
	go srv.ListenAndServe()
	<-ctx.Done()

	// Stop in this order: the requests and the writer, so every accepted
	// write is in the file; the final sync, on a context that is still
	// alive; then a checkpoint, and the database closes.
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	stopJobs()
	err = w.Close()
	err = errors.Join(err, detach(context.Background()))
	if _, e := db.W.ExecContext(sctx, "PRAGMA wal_checkpoint(TRUNCATE)"); e != nil {
		err = errors.Join(err, e)
	}
	return errors.Join(err, db.Close())
}
