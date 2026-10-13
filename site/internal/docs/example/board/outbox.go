package board

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"
)

// Outbox sends the mail that commands wrote into the outbox table, outside
// the writer: the network never holds up a batch. A mail whose row the
// sender could not delete goes out again on the next pass.
type Outbox struct {
	db   *sqlite.DB
	w    *writer.Writer
	send func(ctx context.Context, to, subject string) error
	log  *slog.Logger
	wake chan struct{}
}

// NewOutbox returns a sender that hands each mail to send, and logs its
// failures to log (slog.Default() if nil).
func NewOutbox(db *sqlite.DB, w *writer.Writer, send func(ctx context.Context, to, subject string) error, log *slog.Logger) *Outbox {
	if log == nil {
		log = slog.Default()
	}
	return &Outbox{db: db, w: w, send: send, log: log, wake: make(chan struct{}, 1)}
}

// Wake asks the sender to look at the outbox now. It never blocks, so a
// command can pass it to tx.AfterCommit.
func (o *Outbox) Wake() {
	select {
	case o.wake <- struct{}{}:
	default: // a pass is due already
	}
}

// Run sends what the outbox holds whenever Wake is called, and once a
// minute besides, until ctx ends. Start it from main, like every job.
func (o *Outbox) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if err := o.flush(ctx); err != nil && ctx.Err() == nil {
			o.log.Warn("outbox", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-o.wake:
		case <-t.C:
		}
	}
}

type mail struct {
	id          int64
	to, subject string
}

// flush sends each waiting mail, then deletes its row with a command.
func (o *Outbox) flush(ctx context.Context) error {
	waiting, err := o.db.ViewValue(ctx, func(tx *sql.Tx) ([]mail, error) {
		rows, err := tx.QueryContext(ctx, `SELECT id, to_addr, subject FROM outbox ORDER BY id LIMIT 100`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var ms []mail
		for rows.Next() {
			var m mail
			if err := rows.Scan(&m.id, &m.to, &m.subject); err != nil {
				return nil, err
			}
			ms = append(ms, m)
		}
		return ms, rows.Err()
	})
	if err != nil {
		return err
	}
	for _, m := range waiting {
		if err := o.send(ctx, m.to, m.subject); err != nil {
			return err
		}
		sent := writer.Func(func(tx *writer.Tx) error {
			_, err := tx.Exec(`DELETE FROM outbox WHERE id = ?`, m.id)
			return err
		})
		if err := o.w.Do(ctx, sent); err != nil {
			return err
		}
	}
	return nil
}
