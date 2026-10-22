package board

import (
	"context"
	"log/slog"
	"maps"
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

// OpenCount is what CountOpen publishes: each person's open cards. Every
// subscriber gets the same map, so nobody changes it.
type OpenCount map[string]int

// A WorkloadChange is a value Workload follows: Assigned or OpenCount.
type WorkloadChange interface{ workloadChange() }

func (Assigned) workloadChange()  {}
func (OpenCount) workloadChange() {}

// CountOpen counts each person's open cards in the writer's transaction and
// publishes the count, so that it reaches the subscribers in commit order
// with the Assigned values: the count holds every assignment before it,
// and none after.
type CountOpen struct{}

func (CountOpen) Apply(tx *writer.Tx) error {
	open, err := OpenCards(tx.Context(), tx)
	if err != nil {
		return err
	}
	tx.Publish(OpenCount(open))
	return nil
}

// Workload keeps each person's open cards in memory, for a metric. A count
// replaces what it holds, and the Assigned values after the count move
// cards from one person to another. Between counts it is exact unless its
// queue dropped a value, or a change published none, such as an archive;
// the next count puts both right.
type Workload struct {
	mu   sync.Mutex
	open map[string]int // nil until the first count arrives
}

// Open returns how many open cards who has.
func (l *Workload) Open(who string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.open[who]
}

// Run asks the writer for a count at once and every interval, and follows
// the counts and the Assigned values until ctx ends or the writer stops.
func (l *Workload) Run(ctx context.Context, w *writer.Writer, every time.Duration, log *slog.Logger) {
	changes := w.Subscribe[WorkloadChange](ctx, 256) // before the first count, so that it arrives
	count := func() {
		if err := w.Send(CountOpen{}); err != nil {
			log.Warn("workload: count", "err", err)
		}
	}
	count()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case c, ok := <-changes:
			if !ok {
				return
			}
			l.apply(c)
		case <-t.C:
			count()
		}
	}
}

func (l *Workload) apply(c WorkloadChange) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch c := c.(type) {
	case OpenCount:
		l.open = maps.Clone(c) // the count is shared, and this copy changes
	case Assigned:
		if l.open == nil {
			return // the first count, still to come, holds it
		}
		l.open[c.From]--
		l.open[c.To]++
		delete(l.open, "")
	}
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
