package board

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"
)

// Assigned is what Assign publishes once it commits.
type Assigned struct {
	Card     int64
	From, To string // From is empty for a card nobody had
}

// Workload keeps each person's open cards in memory, for a metric. The
// Assigned values keep it current, and a count from the cards table
// corrects it now and then: a value its full queue dropped, and changes
// that publish none, such as an archive, are right again after the next
// count.
type Workload struct {
	mu   sync.Mutex
	open map[string]int
}

// Open returns how many open cards who has.
func (l *Workload) Open(who string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.open[who]
}

// Run follows the writer's Assigned values, and counts from the database
// every interval, until ctx ends or the writer stops.
func (l *Workload) Run(ctx context.Context, db *sqlite.DB, w *writer.Writer, every time.Duration, log *slog.Logger) {
	assigned := w.Subscribe[Assigned](ctx, 256) // before the count, so that no commit falls between
	t := time.NewTicker(every)
	defer t.Stop()
	l.count(ctx, db, log)
	for {
		select {
		case a, ok := <-assigned:
			if !ok {
				return
			}
			l.mu.Lock()
			if l.open == nil { // no count went through yet
				l.open = map[string]int{}
			}
			l.open[a.From]--
			l.open[a.To]++
			delete(l.open, "")
			l.mu.Unlock()
		case <-t.C:
			l.count(ctx, db, log)
		}
	}
}

func (l *Workload) count(ctx context.Context, db *sqlite.DB, log *slog.Logger) {
	open, err := db.ViewValue(ctx, func(tx *sql.Tx) (map[string]int, error) { return OpenCards(ctx, tx) })
	if err != nil {
		log.Warn("workload: count", "err", err)
		return
	}
	l.mu.Lock()
	l.open = open
	l.mu.Unlock()
}

// OpenCards counts each person's cards on the boards.
func OpenCards(ctx context.Context, q sqlite.Querier) (map[string]int, error) {
	rows, err := q.QueryContext(ctx, `SELECT assignee, count(*) FROM cards
		WHERE assignee IS NOT NULL AND archived IS NULL GROUP BY assignee`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	open := map[string]int{}
	for rows.Next() {
		var who string
		var n int
		if err := rows.Scan(&who, &n); err != nil {
			return nil, err
		}
		open[who] = n
	}
	return open, rows.Err()
}
