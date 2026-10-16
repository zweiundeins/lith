package events

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"
)

// Options tune a Runner; zero fields take the defaults.
type Options struct {
	// Batch is how many events one rebuild command applies, and how many
	// a catch-up projector reads at a time (default 500).
	Batch int
	// Retries is how many times a catch-up projector tries an event
	// before it parks (default 8).
	Retries int
	// Backoff is the wait after a catch-up projector's first failure on
	// an event, doubled after each further one up to MaxBackoff (defaults
	// 1s and 1 min): with the default Retries, an event that keeps failing
	// parks its projector after about two minutes.
	Backoff, MaxBackoff time.Duration
	// Log gets rebuilds, failures and parked projectors (default
	// slog.Default()).
	Log *slog.Logger
}

// Runner keeps a store's projectors current in one database: it rebuilds
// the inline projectors whose version changed, and runs the catch-up
// projectors after the commits that append their events. Without a
// running Runner, inline projectors still run in every command, but no
// rebuild happens and no catch-up projector moves.
type Runner struct {
	st   *Store
	db   *sqlite.DB
	w    *writer.Writer
	opts Options

	started atomic.Bool
	mu      sync.Mutex
	stats   map[string]*ProjectorStats
	resume  map[string]chan struct{}
}

// Stats are a runner's projectors as they stand.
type Stats struct {
	Projectors []ProjectorStats // by name
}

// ProjectorStats is one projector in a database.
type ProjectorStats struct {
	Name    string
	Version int
	CatchUp bool
	// Status is "live", "rebuilding" or "parked".
	Status string
	// Position is a catch-up projector's checkpoint, the last event it
	// handled, or how far a rebuild got.
	Position int64
	// Error is why a projector parked, or the failure of the event it
	// retries; it is cleared once the event goes through.
	Error string
	// Attempts counts the failures of the event a catch-up projector
	// retries.
	Attempts int
}

// NewRunner makes a runner of st's projectors on db, whose writer is w.
func NewRunner(st *Store, db *sqlite.DB, w *writer.Writer, opts Options) *Runner {
	if opts.Batch <= 0 {
		opts.Batch = 500
	}
	if opts.Retries <= 0 {
		opts.Retries = 8
	}
	if opts.Backoff <= 0 {
		opts.Backoff = time.Second
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = time.Minute
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Runner{st: st, db: db, w: w, opts: opts, stats: map[string]*ProjectorStats{}, resume: map[string]chan struct{}{}}
}

// Run starts the runner's work and returns when ctx ends, once the work
// under way has stopped. At its start, in one command, it creates the
// tables, records new projectors and changed versions, and tries parked
// projectors again; an error there, such as a projector's missing table,
// is returned. Rebuilds then run one after the other, and each catch-up
// projector on its own goroutine. Cancel ctx before closing the writer.
func (r *Runner) Run(ctx context.Context) error {
	if !r.started.CompareAndSwap(false, true) {
		return errors.New("events: Run called twice")
	}
	if err := r.st.ready(); err != nil {
		return err
	}
	wakes := map[string][]chan struct{}{}
	chans := map[string]chan struct{}{}
	for _, p := range r.st.projectors {
		if p.catchUp {
			c := make(chan struct{}, 1)
			chans[p.name] = c
			wakes[p.kind] = append(wakes[p.kind], c)
			r.resume[p.name] = make(chan struct{}, 1)
		}
	}
	wake := map[string]func(){}
	for kind, cs := range wakes {
		wake[kind] = func() {
			for _, c := range cs {
				select {
				case c <- struct{}{}:
				default:
				}
			}
		}
	}
	rows, err := r.setup(ctx, wake)
	if err != nil {
		return err
	}
	var rebuild []*projector
	var wg sync.WaitGroup
	for _, p := range r.st.projectors {
		row := rows[p.name]
		r.mu.Lock()
		r.stats[p.name] = &ProjectorStats{Name: p.name, Version: p.version, CatchUp: p.catchUp, Status: row.status, Position: row.position}
		r.mu.Unlock()
		switch {
		case p.catchUp:
			wg.Go(func() { r.catchUp(ctx, p, row.position, chans[p.name]) })
		case row.status == statusRebuilding:
			rebuild = append(rebuild, p)
		}
	}
	wg.Go(func() {
		for _, p := range rebuild {
			r.rebuild(ctx, p)
		}
	})
	<-ctx.Done()
	wg.Wait()
	r.w.Do(context.WithoutCancel(ctx), writer.Func(func(tx *writer.Tx) error {
		r.st.local(tx).wake = nil
		return nil
	}))
	return nil
}

type row struct {
	status   string
	position int64
}

// setup prepares the database for the runner and reads its projectors.
func (r *Runner) setup(ctx context.Context, wake map[string]func()) (map[string]row, error) {
	rows := map[string]row{}
	err := r.w.Do(ctx, writer.Func(func(tx *writer.Tx) error {
		clear(rows)
		l := r.st.local(tx)
		if err := r.st.tables(tx, l); err != nil {
			return err
		}
		l.forget()
		if _, err := r.st.running(tx, l); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE lith_projections SET status = ? WHERE status LIKE 'parked:%'`, statusLive); err != nil {
			return err
		}
		for _, p := range r.st.projectors {
			for _, t := range p.tables {
				if _, err := tx.ExecUncached(`DROP TABLE IF EXISTS ` + quote(t+"__retired")); err != nil {
					return err
				}
			}
			var rw row
			err := tx.QueryRow(`SELECT status, position FROM lith_projections WHERE name = ?`, p.name).Scan(&rw.status, &rw.position)
			if err != nil {
				return err
			}
			rows[p.name] = rw
		}
		l.wake = wake
		return nil
	}))
	return rows, err
}

func (r *Runner) note(name string, fn func(s *ProjectorStats)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.stats[name]; s != nil {
		fn(s)
	}
}

// Stats returns the runner's projectors as they stand.
func (r *Runner) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Stats{}
	for _, p := range r.stats {
		s.Projectors = append(s.Projectors, *p)
	}
	slices.SortFunc(s.Projectors, func(a, b ProjectorStats) int { return strings.Compare(a.Name, b.Name) })
	return s
}

// Resume has a parked catch-up projector try its event again.
func (r *Runner) Resume(name string) error {
	r.mu.Lock()
	c := r.resume[name]
	r.mu.Unlock()
	if c == nil {
		return fmt.Errorf("events: no catch-up projector %s runs here", name)
	}
	select {
	case c <- struct{}{}:
	default:
	}
	return nil
}

// sleep waits for d, or until ctx ends.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (r *Runner) backoff(attempt int) time.Duration {
	d := r.opts.Backoff
	for range attempt - 1 {
		if d *= 2; d >= r.opts.MaxBackoff {
			return r.opts.MaxBackoff
		}
	}
	return d
}

// rebuild fills copies of an inline projector's tables from every event
// of its stream, a command per Batch events, and puts them in place.
func (r *Runner) rebuild(ctx context.Context, p *projector) {
	log := r.opts.Log.With("projector", p.name, "version", p.version)
	log.Info("events: rebuild starts")
	start := time.Now()
	var pos int64
	live := false
	for attempt := 1; ; attempt++ {
		err := r.w.Do(ctx, writer.Func(func(tx *writer.Tx) (err error) {
			pos, live, err = r.begin(tx, p)
			return err
		}))
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		log.Error("events: rebuild cannot start", "err", err)
		r.note(p.name, func(s *ProjectorStats) { s.Error = err.Error() })
		if !sleep(ctx, r.backoff(attempt)) {
			return
		}
	}
	for attempt := 1; !live; {
		var next int64
		var done bool
		err := r.w.Do(ctx, writer.Func(func(tx *writer.Tx) (err error) {
			next, done, err = r.batch(tx, p, pos)
			return err
		}))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("events: rebuild batch failed", "position", pos, "err", err)
			r.note(p.name, func(s *ProjectorStats) { s.Error = err.Error() })
			if !sleep(ctx, r.backoff(attempt)) {
				return
			}
			attempt++
			continue
		}
		pos, live, attempt = next, done, 1
		r.note(p.name, func(s *ProjectorStats) {
			s.Position, s.Error = pos, ""
			if done {
				s.Status = statusLive
			}
		})
	}
	err := r.w.Do(context.WithoutCancel(ctx), writer.Func(func(tx *writer.Tx) error {
		for _, t := range p.tables {
			if _, err := tx.ExecUncached(`DROP TABLE IF EXISTS ` + quote(t+"__retired")); err != nil {
				return err
			}
		}
		return nil
	}))
	if err != nil {
		log.Warn("events: drop the tables a rebuild replaced", "err", err)
	}
	log.Info("events: rebuild done", "position", pos, "took", time.Since(start).Round(time.Millisecond))
}

// begin makes the copies of p's tables that a rebuild fills, unless they
// exist from a rebuild that stopped, and returns where it stands. live is
// true when there is nothing to rebuild.
func (r *Runner) begin(tx *writer.Tx, p *projector) (pos int64, live bool, err error) {
	var version int
	var status string
	err = tx.QueryRow(`SELECT version, position, status FROM lith_projections WHERE name = ?`, p.name).Scan(&version, &pos, &status)
	if err != nil {
		return 0, false, err
	}
	if version != p.version || status != statusRebuilding {
		return pos, true, nil
	}
	var tables []string
	rows, err := tx.QueryUncached(`SELECT name FROM sqlite_schema WHERE type = 'table'`)
	if err != nil {
		return 0, false, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return 0, false, err
		}
		tables = append(tables, n)
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	rows.Close()
	reset := false
	for _, t := range p.tables {
		copy := shadowName(t, p.version)
		exists := false
		for _, n := range tables {
			switch {
			case strings.EqualFold(n, copy):
				exists = true
			case versioned.MatchString(n) && strings.EqualFold(versioned.ReplaceAllString(n, ""), t),
				strings.EqualFold(n, t+"__retired"), strings.EqualFold(n, t+"__hold"):
				// a copy for another version, or what a crash left
				if _, err := tx.ExecUncached(`DROP TABLE ` + quote(n)); err != nil {
					return 0, false, err
				}
			}
		}
		if exists {
			continue
		}
		reset = true
		if err := createCopy(tx, t, copy, p.version); err != nil {
			return 0, false, fmt.Errorf("events: projector %s: %w", p.name, err)
		}
	}
	if reset && pos != 0 {
		pos = 0
		if _, err := tx.Exec(`UPDATE lith_projections SET position = 0 WHERE name = ?`, p.name); err != nil {
			return 0, false, err
		}
	}
	return pos, false, nil
}

// createCopy creates an empty copy of table t, with its indexes, named
// copy.
func createCopy(tx *writer.Tx, t, copy string, version int) error {
	rows, err := tx.QueryUncached(`SELECT type, sql FROM sqlite_schema
		WHERE tbl_name = ? COLLATE NOCASE AND sql IS NOT NULL ORDER BY type != 'table'`, t)
	if err != nil {
		return err
	}
	var ddl []string
	for rows.Next() {
		var typ, sql string
		if err := rows.Scan(&typ, &sql); err != nil {
			return err
		}
		if typ != "table" && typ != "index" {
			return fmt.Errorf("table %s has a %s: a projection's tables have none", t, typ)
		}
		q, err := shadowDDL(sql, copy, version)
		if err != nil {
			return err
		}
		ddl = append(ddl, q)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	if len(ddl) == 0 {
		return fmt.Errorf("no table %s: create it in a migration", t)
	}
	for _, q := range ddl {
		if _, err := tx.ExecUncached(q); err != nil {
			return fmt.Errorf("copy table %s: %w", t, err)
		}
	}
	return nil
}

// batch applies the next Batch events of p's stream to the copies of its
// tables. The handlers write the tables by their names, so for the length
// of the command each copy takes its table's name, and the table steps
// aside. The batch that finds fewer events left is the swap: it applies
// them and leaves the copies in place, the tables they replace renamed
// for a later command to drop. Readers see the old tables until the swap
// commits, and the new ones from then on.
func (r *Runner) batch(tx *writer.Tx, p *projector, pos int64) (next int64, done bool, err error) {
	evs, err := page(tx.Context(), tx, p.kind, pos, r.opts.Batch)
	if err != nil {
		return pos, false, err
	}
	done = len(evs) < r.opts.Batch
	if done {
		r.st.local(tx).forget()
	}
	restore, err := legacyAlter(tx)
	if err != nil {
		return pos, false, err
	}
	defer func() { err = errors.Join(err, restore()) }()
	aside := "__hold"
	if done {
		aside = "__retired"
	}
	for _, t := range p.tables {
		if err := rename(tx, t, t+aside); err != nil {
			return pos, false, err
		}
		if err := rename(tx, shadowName(t, p.version), t); err != nil {
			return pos, false, err
		}
	}
	if err := p.replay(r.st, tx, evs); err != nil {
		return pos, false, err
	}
	next = pos
	if len(evs) > 0 {
		next = evs[len(evs)-1].Position
	}
	if done {
		if _, err := tx.Exec(`UPDATE lith_projections SET position = ?, status = ? WHERE name = ?`, next, statusLive, p.name); err != nil {
			return pos, false, err
		}
		tx.Touch(p.topic)
		return next, true, nil
	}
	for _, t := range p.tables {
		if err := rename(tx, t, shadowName(t, p.version)); err != nil {
			return pos, false, err
		}
		if err := rename(tx, t+aside, t); err != nil {
			return pos, false, err
		}
	}
	_, err = tx.Exec(`UPDATE lith_projections SET position = ? WHERE name = ?`, next, p.name)
	return next, false, err
}

func rename(tx *writer.Tx, from, to string) error {
	_, err := tx.ExecUncached(`ALTER TABLE ` + quote(from) + ` RENAME TO ` + quote(to))
	return err
}

// legacyAlter has ALTER TABLE RENAME leave the views, triggers and foreign
// keys that name a table as they are: they go on naming the table that
// holds the name. It returns what turns it off again.
func legacyAlter(tx *writer.Tx) (func() error, error) {
	var on int
	if err := tx.QueryRow(`PRAGMA legacy_alter_table`).Scan(&on); err != nil {
		return nil, err
	}
	if on == 1 {
		return func() error { return nil }, nil
	}
	if _, err := tx.Exec(`PRAGMA legacy_alter_table = ON`); err != nil {
		return nil, err
	}
	return func() error {
		_, err := tx.Exec(`PRAGMA legacy_alter_table = OFF`)
		return err
	}, nil
}

// catchUp runs a catch-up projector from position pos until ctx ends.
func (r *Runner) catchUp(ctx context.Context, p *projector, pos int64, wake chan struct{}) {
	log := r.opts.Log.With("projector", p.name)
	r.mu.Lock()
	resume := r.resume[p.name]
	r.mu.Unlock()
	parked := false
	for ctx.Err() == nil {
		if parked {
			select {
			case <-resume:
			case <-ctx.Done():
				return
			}
			if err := r.status(ctx, p, statusLive, pos); err != nil {
				log.Error("events: resume", "err", err)
				continue
			}
			parked = false
			r.note(p.name, func(s *ProjectorStats) { s.Status, s.Attempts = statusLive, 0 })
		}
		var evs []Raw
		err := r.db.View(ctx, func(tx *sql.Tx) (err error) {
			evs, err = page(ctx, tx, p.kind, pos, r.opts.Batch)
			return err
		})
		if err != nil {
			if ctx.Err() == nil {
				log.Error("events: read the events", "err", err)
				sleep(ctx, r.opts.Backoff)
			}
			continue
		}
		if len(evs) == 0 {
			select {
			case <-wake:
			case <-ctx.Done():
			}
			continue
		}
		done := pos
		var failed error
		for _, ev := range evs {
			if failed = r.try(ctx, p, ev, log); failed != nil {
				break
			}
			done = ev.Position
		}
		if done > pos {
			pos = done
			if err := r.checkpoint(ctx, p, pos); err != nil {
				log.Error("events: checkpoint", "position", pos, "err", err)
			}
			r.note(p.name, func(s *ProjectorStats) { s.Position = pos })
		}
		if failed != nil && ctx.Err() == nil {
			msg := failed.Error()
			if len(msg) > 1000 {
				msg = msg[:1000]
			}
			log.Error("events: projector parked", "position", pos, "err", failed)
			if err := r.status(ctx, p, parkedPrefix+msg, pos); err != nil {
				log.Error("events: park", "err", err)
			}
			parked = true
			r.note(p.name, func(s *ProjectorStats) { s.Status = "parked" })
		}
	}
}

// try hands ev to p until it goes through, waiting longer after each
// failure; after Retries failures it returns the last error.
func (r *Runner) try(ctx context.Context, p *projector, ev Raw, log *slog.Logger) error {
	for attempt := 1; ; attempt++ {
		err := p.deliver(ctx, r.st, ev)
		if err == nil {
			r.note(p.name, func(s *ProjectorStats) { s.Error, s.Attempts = "", 0 })
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.note(p.name, func(s *ProjectorStats) { s.Error, s.Attempts = err.Error(), attempt })
		if attempt >= r.opts.Retries {
			return err
		}
		log.Warn("events: projector failed, retrying", "position", ev.Position, "attempt", attempt, "err", err)
		if !sleep(ctx, r.backoff(attempt)) {
			return ctx.Err()
		}
	}
}

// checkpoint records that p handled every event up to pos. It goes in
// also when ctx has just ended, as the runner stops.
func (r *Runner) checkpoint(ctx context.Context, p *projector, pos int64) error {
	return r.w.Do(context.WithoutCancel(ctx), writer.Func(func(tx *writer.Tx) error {
		_, err := tx.Exec(`UPDATE lith_projections SET position = ? WHERE name = ? AND version = ?`, pos, p.name, p.version)
		return err
	}))
}

func (r *Runner) status(ctx context.Context, p *projector, status string, pos int64) error {
	return r.w.Do(context.WithoutCancel(ctx), writer.Func(func(tx *writer.Tx) error {
		_, err := tx.Exec(`UPDATE lith_projections SET status = ?, position = ? WHERE name = ? AND version = ?`, status, pos, p.name, p.version)
		return err
	}))
}
