package events_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"
)

// The accounts: a stream per account, whose balance is the sum of all its
// moves, so that a rebuild that misses the oldest gets every balance
// wrong; and their snapshot, Balanced, which Compact writes in their place.

type AccountEvent interface{ accountEvent() }

type Deposited struct {
	Amount int64 `json:"amount"`
}

type Withdrawn struct {
	Amount int64 `json:"amount"`
}

// Balanced is an account's snapshot: its balance and its number of moves
// as of the event Compact rewrote.
type Balanced struct {
	Balance int64 `json:"balance"`
	Moves   int64 `json:"moves"`
}

func (Deposited) accountEvent() {}
func (Withdrawn) accountEvent() {}
func (Balanced) accountEvent()  {}

var Accounts = events.NewStream[int64, AccountEvent]("account")

func defineAccount[T AccountEvent](name string) events.Type[int64, AccountEvent, T] {
	return events.Define(Accounts, name, func(t T) AccountEvent { return t })
}

var (
	AccountDeposited = defineAccount[Deposited]("account.deposited")
	AccountWithdrawn = defineAccount[Withdrawn]("account.withdrawn")
	AccountBalanced  = defineAccount[Balanced]("account.balanced")
)

type account struct {
	Balance, Moves int64
}

func evolveAccount(s account, e AccountEvent) account {
	switch e := e.(type) {
	case Deposited:
		s.Balance += e.Amount
		s.Moves++
	case Withdrawn:
		s.Balance -= e.Amount
		s.Moves++
	case Balanced:
		return account{Balance: e.Balance, Moves: e.Moves}
	}
	return s
}

// Move deposits a positive amount and withdraws a negative one, as far as
// the balance goes.
type Move struct{ Amount int64 }

var move = events.Decider[Move, AccountEvent, account]{
	Decide: func(c Move, s account) ([]AccountEvent, error) {
		switch {
		case c.Amount > 0:
			return []AccountEvent{Deposited{Amount: c.Amount}}, nil
		case c.Amount < 0 && s.Balance >= -c.Amount:
			return []AccountEvent{Withdrawn{Amount: -c.Amount}}, nil
		}
		return nil, nil
	},
	Evolve:  evolveAccount,
	Version: 1,
}

func balancedOf(s account) Balanced { return Balanced{Balance: s.Balance, Moves: s.Moves} }

// balances keeps each account's balance now, with the version and the
// actor of its last event.
func balances(version int) *events.Projector[int64, AccountEvent] {
	return events.NewProjector(Accounts, "balances", version,
		events.Handle(AccountDeposited, func(tx *writer.Tx, r events.Record[int64, Deposited]) error {
			return addBalance(tx, r.ID, r.Data.Amount, r.Version, r.Actor)
		}),
		events.Handle(AccountWithdrawn, func(tx *writer.Tx, r events.Record[int64, Withdrawn]) error {
			return addBalance(tx, r.ID, -r.Data.Amount, r.Version, r.Actor)
		}),
		events.Handle(AccountBalanced, func(tx *writer.Tx, r events.Record[int64, Balanced]) error {
			_, err := tx.Exec(`INSERT INTO balances (id, balance, moves, version, actor) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (id) DO UPDATE SET balance = excluded.balance, moves = excluded.moves, version = excluded.version, actor = excluded.actor`,
				r.ID, r.Data.Balance, r.Data.Moves, r.Version, r.Actor)
			return err
		}),
	).Tables("balances")
}

func addBalance(tx *writer.Tx, id, amount, version int64, actor string) error {
	_, err := tx.Exec(`INSERT INTO balances (id, balance, moves, version, actor) VALUES (?1, ?2, 1, ?3, ?4)
		ON CONFLICT (id) DO UPDATE SET balance = balance + ?2, moves = moves + 1, version = ?3, actor = ?4`, id, amount, version, actor)
	return err
}

// balanceHistory keeps every balance an account had, with the positions
// it held between.
func balanceHistory(version int) *events.Projector[int64, AccountEvent] {
	return events.NewProjector(Accounts, "balance_history", version,
		events.Handle(AccountDeposited, func(tx *writer.Tx, r events.Record[int64, Deposited]) error {
			return nextBalance(tx, r.ID, r.Position, r.Actor, r.Data.Amount, false)
		}),
		events.Handle(AccountWithdrawn, func(tx *writer.Tx, r events.Record[int64, Withdrawn]) error {
			return nextBalance(tx, r.ID, r.Position, r.Actor, -r.Data.Amount, false)
		}),
		events.Handle(AccountBalanced, func(tx *writer.Tx, r events.Record[int64, Balanced]) error {
			return nextBalance(tx, r.ID, r.Position, r.Actor, r.Data.Balance, true)
		}),
	).Tables("balance_history")
}

// nextBalance ends an account's current row at position pos and starts
// the next one there, its balance moved by amount, or set to it.
func nextBalance(tx *writer.Tx, id, pos int64, actor string, amount int64, set bool) error {
	var was int64
	err := tx.QueryRow(`SELECT balance FROM balance_history WHERE account_id = ? AND valid_to IS NULL`, id).Scan(&was)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.Exec(`UPDATE balance_history SET valid_to = ? WHERE account_id = ? AND valid_to IS NULL`, pos, id); err != nil {
		return err
	}
	if !set {
		amount += was
	}
	_, err = tx.Exec(`INSERT INTO balance_history (account_id, balance, actor, valid_from) VALUES (?, ?, ?, ?)`, id, amount, actor, pos)
	return err
}

// balancesAt reads every account's balance as of position pos from the
// history table.
func balancesAt(ctx context.Context, q sqlite.Querier, pos int64) (map[int64]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT account_id, balance FROM balance_history
		WHERE valid_from <= ?1 AND (valid_to IS NULL OR valid_to > ?1)`, pos)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]int64{}
	for rows.Next() {
		var id, balance int64
		if err := rows.Scan(&id, &balance); err != nil {
			return nil, err
		}
		m[id] = balance
	}
	return m, rows.Err()
}

var actors = []string{"ada", "bob", "cy"}

// moveMany makes n random moves on the accounts 1 to accounts, by three
// actors, in one command. A withdrawal the balance does not cover appends
// nothing.
func (f *fixture) moveMany(t testing.TB, d events.Decider[Move, AccountEvent, account], rnd *rand.Rand, n, accounts int) {
	t.Helper()
	if err := f.do(t, func(tx *writer.Tx) error {
		for range n {
			id := Accounts.ID(1 + rnd.Int64N(int64(accounts)))
			err := events.Execute(f.st, tx, d, id, events.Any, Move{Amount: rnd.Int64N(200) - 80},
				events.Meta{Actor: actors[rnd.IntN(len(actors))]})
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// accountsByPosition replays the log into every account's state after
// each position.
func (f *fixture) accountsByPosition(t testing.TB) map[int64]map[int64]account {
	t.Helper()
	ctx := context.Background()
	by := map[int64]map[int64]account{}
	err := f.db.View(ctx, func(tx *sql.Tx) error {
		now := map[int64]account{}
		for raw, err := range events.SinceUntil(ctx, tx, 0, math.MaxInt64) {
			if err != nil {
				return err
			}
			e, err := events.Decode(f.st, Accounts, raw.Type, raw.Data)
			if err != nil {
				return err
			}
			id := raw.ID.(int64)
			now[id] = evolveAccount(now[id], e)
			by[raw.Position] = maps.Clone(now)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return by
}

// checkAccountsAt checks every position from first to last against want:
// the history table, a replay of the log up to the position, and each
// account's StateAt.
func (f *fixture) checkAccountsAt(t testing.TB, want map[int64]map[int64]account, first, last int64, accounts int) {
	t.Helper()
	ctx := context.Background()
	err := f.db.View(ctx, func(tx *sql.Tx) error {
		for p := first; p <= last; p++ {
			table, err := balancesAt(ctx, tx, p)
			if err != nil {
				return err
			}
			if len(table) != len(want[p]) {
				return fmt.Errorf("at %d the history table has %v, want %v", p, table, want[p])
			}
			for id := range int64(accounts) {
				id++
				s, _, err := events.StateAt(ctx, f.st, tx, move, Accounts.ID(id), p)
				if err != nil {
					return err
				}
				if s != want[p][id] || table[id] != want[p][id].Balance {
					return fmt.Errorf("at %d account %d is %+v, in the history table %d, want %+v", p, id, s, table[id], want[p][id])
				}
			}
		}
		now := map[int64]account{}
		for raw, err := range events.SinceUntil(ctx, tx, 0, last) {
			if err != nil {
				return err
			}
			e, err := events.Decode(f.st, Accounts, raw.Type, raw.Data)
			if err != nil {
				return err
			}
			now[raw.ID.(int64)] = evolveAccount(now[raw.ID.(int64)], e)
			if raw.Position >= first && !maps.Equal(now, want[raw.Position]) {
				return fmt.Errorf("at %d the replay has %v, want %v", raw.Position, now, want[raw.Position])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// lines reads the rows of a query as text, a line per row.
func (f *fixture) lines(t testing.TB, query string, args ...any) []string {
	t.Helper()
	rows, err := f.db.R.Query(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		out = append(out, strings.TrimSuffix(fmt.Sprintln(vals...), "\n"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *fixture) bounds(t testing.TB, s *events.Stream[int64, AccountEvent]) (first, last int64) {
	t.Helper()
	err := f.db.View(context.Background(), func(tx *sql.Tx) (err error) {
		first, last, err = events.Bounds(context.Background(), tx, s)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return first, last
}

func (f *fixture) compact(t testing.TB, d events.Decider[Move, AccountEvent, account], keep events.Retention) int64 {
	t.Helper()
	var n int64
	if err := f.do(t, func(tx *writer.Tx) (err error) {
		n, err = events.Compact(f.st, tx, AccountBalanced, keep, d, balancedOf)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

const (
	balanceRows = `SELECT id, balance, moves, version, actor FROM balances ORDER BY id`
	historyRows = `SELECT account_id, balance, actor, valid_from, valid_to FROM balance_history
		WHERE valid_to IS NULL OR valid_to > ? ORDER BY account_id, valid_from`
	eventRows = `SELECT position, stream_id, version, type, data, at, actor FROM lith_events ORDER BY position`
)

// Compact keeps an account's state in a snapshot where its oldest events
// were: each account that lost events keeps one, its last, rewritten in
// place. From the newest snapshot on, the history table, a replay of the
// log and StateAt read what they read before; a rebuild makes the tables
// it had; and commands, also of a decider whose Version changed, decide
// on the balances as they were.
func TestCompact(t *testing.T) {
	const accounts = 6
	f := open(t, balances(1), balanceHistory(1))
	rnd := rand.New(rand.NewPCG(5, 6))
	f.moveMany(t, move, rnd, 400, accounts)
	head := f.maxPosition(t)
	want := f.accountsByPosition(t)
	was := f.lines(t, eventRows)
	tables := [][]string{f.lines(t, balanceRows)}

	n := f.compact(t, move, events.Retention{Newest: 100})
	first, last := f.bounds(t, Accounts)
	if last != head || first != head-100 {
		t.Fatalf("bounds %d to %d after compacting %d events to the newest 100", first, last, head)
	}
	tables = append(tables, f.lines(t, historyRows, first))

	// Before the newest 100: a snapshot of each account that lost events,
	// at its last event's position, with its version, time and actor.
	now := f.lines(t, eventRows)
	snapshots := now[:len(now)-100]
	if n != head-100-int64(len(snapshots)) || len(snapshots) == 0 || len(snapshots) > accounts {
		t.Fatalf("deleted %d of %d events, kept %d snapshots", n, head, len(snapshots))
	}
	if !slices.Equal(now[len(snapshots):], was[head-100:]) {
		t.Error("the newest 100 events changed")
	}
	seen := map[string]bool{}
	for _, line := range snapshots {
		var pos, id, version int64
		var typ string
		fmt.Sscan(line, &pos, &id, &version, &typ)
		if typ != "account.balanced" || seen[fmt.Sprint(id)] {
			t.Errorf("before the newest 100: %s", line)
		}
		seen[fmt.Sprint(id)] = true
		orig := strings.Fields(was[pos-1])
		got := strings.Fields(line)
		if orig[2] != got[2] || orig[len(orig)-2] != got[len(got)-2] || orig[len(orig)-1] != got[len(got)-1] {
			t.Errorf("the snapshot %s of %s", line, was[pos-1])
		}
		for _, later := range was[pos:first] {
			if strings.Fields(later)[1] == fmt.Sprint(id) {
				t.Errorf("account %d's snapshot at %d is not its last event before %d", id, pos, first)
			}
		}
		if want[pos][id] != want[first][id] {
			t.Errorf("account %d changed between its snapshot and %d", id, first)
		}
	}
	f.checkAccountsAt(t, want, first, last, accounts)

	// Before the newest snapshot, an account whose snapshot comes later
	// has no state.
	var pos, id int64
	fmt.Sscan(snapshots[len(snapshots)-1], &pos, &id)
	err := f.db.View(context.Background(), func(tx *sql.Tx) error {
		_, _, err := events.StateAt(context.Background(), f.st, tx, move, Accounts.ID(id), pos-1)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "retention") {
		t.Errorf("account %d before its snapshot: %v", id, err)
	}

	// Again, with nothing new to close: nothing changes.
	if n := f.compact(t, move, events.Retention{Newest: 100}); n != 0 || !slices.Equal(f.lines(t, eventRows), now) {
		t.Errorf("a second compaction deleted %d events", n)
	}

	// A rebuild of both projectors from the compacted log.
	v2 := f.reopen(balances(2), balanceHistory(2))
	r := v2.run(t, events.Options{Batch: 7})
	waitFor(t, "the rebuilds", func() bool {
		return status(r, "balances").Status == "live" && status(r, "balance_history").Status == "live"
	})
	if got := v2.lines(t, balanceRows); !slices.Equal(got, tables[0]) {
		t.Errorf("balances after the rebuild\n%s\nbefore\n%s", strings.Join(got, "\n"), strings.Join(tables[0], "\n"))
	}
	if got := v2.lines(t, historyRows, 0); !slices.Equal(got, tables[1]) {
		t.Errorf("balance_history after the rebuild\n%s\nbefore, from %d\n%s", strings.Join(got, "\n"), first, strings.Join(tables[1], "\n"))
	}
	v2.checkAccountsAt(t, want, first, last, accounts)

	// A decider of a new Version folds each account again, from its
	// snapshot, and decides on the balance it had.
	move2 := move
	move2.Version = 2
	v2.moveMany(t, move2, rnd, 150, accounts)
	var states []string
	for id := range int64(accounts) {
		states = append(states, fmt.Sprint(id+1, " ", want[last][id+1].Moves))
	}
	refolded := v2.lines(t, `SELECT s.stream_id, json_extract(state, '$.Moves') - count(e.position) FROM lith_event_state s
		LEFT JOIN lith_events e ON e.stream = s.stream AND e.stream_id = s.stream_id AND e.position > ?
		WHERE s.stream = 'account' AND fold = 2 GROUP BY s.stream_id ORDER BY s.stream_id`, last)
	if !slices.Equal(refolded, states) {
		t.Errorf("the moves of the refolded states, less the new ones: %v, want %v", refolded, states)
	}

	// The second compaction replaces the snapshots of the accounts that
	// lost events since.
	want = v2.accountsByPosition(t)
	head = v2.maxPosition(t)
	v2.compact(t, move2, events.Retention{Newest: 50})
	first2, last2 := v2.bounds(t, Accounts)
	if last2 != head || first2 != head-50 {
		t.Fatalf("bounds %d to %d after compacting to the newest 50 of %d", first2, last2, head)
	}
	if got := v2.lines(t, `SELECT stream_id, count(*) FROM lith_events WHERE position <= ? AND type != 'account.balanced' GROUP BY 1
		UNION ALL SELECT stream_id, count(*) FROM lith_events WHERE position <= ? GROUP BY 1 HAVING count(*) > 1`, first2, first2); len(got) != 0 {
		t.Errorf("before the newest snapshot: %v", got)
	}
	v2.checkAccountsAt(t, want, first2, last2, accounts)

	// Every projector handles or ignores the snapshot.
	if _, err := events.New(events.NewProjector(Accounts, "p", 1, events.Ignore(AccountDeposited), events.Ignore(AccountWithdrawn))); err == nil ||
		!strings.Contains(err.Error(), "account.balanced") {
		t.Errorf("a projector without the snapshot: %v", err)
	}
}

// Compact takes at most Limit events a call, and goes no further than a
// catch-up projector of the kind has passed.
func TestCompactLimit(t *testing.T) {
	seen := make(chan int64, 100)
	audit := events.NewProjector(Accounts, "audit", 1,
		events.After(AccountDeposited, func(ctx context.Context, r events.Record[int64, Deposited]) error {
			seen <- r.Position
			return nil
		}),
		events.Ignore(AccountWithdrawn),
		events.Ignore(AccountBalanced),
	)
	f := open(t, balances(1), audit)
	for i := range int64(20) {
		if err := f.do(t, func(tx *writer.Tx) error {
			return events.Execute(f.st, tx, move, Accounts.ID(1+i%2), events.Any, Move{Amount: 10}, events.Meta{})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.compact(t, move, events.Retention{Newest: 5}); n != 0 {
		t.Errorf("compacted %d events the audit has not seen", n)
	}
	f.run(t, events.Options{})
	for range 20 {
		<-seen
	}
	waitFor(t, "the audit's checkpoint", func() bool {
		return f.count(t, `SELECT count(*) FROM lith_projections WHERE name = 'audit' AND position = 20`) == 1
	})
	// Ten of the 15 to go: positions 1 to 10, the last of each account a
	// snapshot.
	if n := f.compact(t, move, events.Retention{Newest: 5, Limit: 10}); n != 8 {
		t.Errorf("the first ten: deleted %d", n)
	}
	if first, _ := f.bounds(t, Accounts); first != 10 {
		t.Errorf("first %d after ten", first)
	}
	// The other five, and the snapshots they replace.
	if n := f.compact(t, move, events.Retention{Newest: 5, Limit: 10}); n != 5 {
		t.Errorf("the next five: deleted %d", n)
	}
	if got := f.lines(t, `SELECT position, stream_id, type FROM lith_events WHERE position <= 15`); !slices.Equal(got, []string{"14 2 account.balanced", "15 1 account.balanced"}) {
		t.Errorf("the snapshots: %v", got)
	}
	if got := f.lines(t, `SELECT data FROM lith_events WHERE position <= 15`); !slices.Equal(got, []string{`{"balance":70,"moves":7}`, `{"balance":80,"moves":8}`}) {
		t.Errorf("the snapshots' data: %v", got)
	}
}

// storedEvent is a row of lith_events.
type storedEvent struct {
	Position, ID, Version, At int64
	Type, Data                string
}

func (f *fixture) storedEvents(t testing.TB) []storedEvent {
	t.Helper()
	rows, err := f.db.R.Query(`SELECT position, stream_id, version, at, type, data FROM lith_events ORDER BY position`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var evs []storedEvent
	for rows.Next() {
		var e storedEvent
		if err := rows.Scan(&e.Position, &e.ID, &e.Version, &e.At, &e.Type, &e.Data); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return evs
}

// Compact with Since and a Limit, between rounds of moves: each call
// leaves the log as the rule says, from the oldest events after the last
// call to the first event kept, the last of each stream among them its
// snapshot, every older event of those streams gone. From the newest
// snapshot on, StateAt and LoadUntil read what the log read before.
func TestCompactSince(t *testing.T) {
	const accounts = 5
	f := open(t, balances(1), balanceHistory(1))
	ctx := context.Background()
	rnd := rand.New(rand.NewPCG(7, 8))
	t0 := time.Now().Add(-time.Hour).UnixMilli()
	var i, since, from int64
	for round := range 8 {
		if err := f.do(t, func(tx *writer.Tx) error {
			for range 60 + rnd.IntN(60) {
				i++
				err := events.Execute(f.st, tx, move, Accounts.ID(1+rnd.Int64N(accounts)), events.Any, Move{Amount: rnd.Int64N(200) - 80},
					events.Meta{At: time.UnixMilli(t0 + i)})
				if err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		want := f.accountsByPosition(t)
		before := f.storedEvents(t)
		since += rnd.Int64N(100)
		limit := []int{0, 1, 7, 40}[round%4]

		// What the call takes: the events after from, before the first
		// stored at or after since, Limit of them at most.
		var taken []storedEvent
		for _, e := range before {
			if e.At >= t0+since {
				break
			}
			if e.Position > from && (limit == 0 || len(taken) < limit) {
				taken = append(taken, e)
			}
		}
		ends := map[int64]int64{}
		for _, e := range taken {
			ends[e.ID] = e.Position
		}
		var kept []storedEvent
		var deleted int64
		for _, e := range before {
			end, ok := ends[e.ID]
			switch {
			case ok && e.Position < end:
				deleted++
				continue
			case ok && e.Position == end:
				s := want[end][e.ID]
				e.Type, e.Data = "account.balanced", fmt.Sprintf(`{"balance":%d,"moves":%d}`, s.Balance, s.Moves)
			}
			kept = append(kept, e)
		}

		n := f.compact(t, move, events.Retention{Since: time.UnixMilli(t0 + since), Limit: limit})
		if got := f.storedEvents(t); n != deleted || !slices.Equal(got, kept) {
			t.Fatalf("round %d: deleted %d events, want %d\n%v\nwant\n%v", round, n, deleted, got, kept)
		}
		if len(taken) > 0 {
			from = taken[len(taken)-1].Position
		}
		if got := f.count(t, `SELECT coalesce(max(compacted), 0) FROM lith_event_kinds`); int64(got) != from {
			t.Errorf("round %d: compacted to %d, want %d", round, got, from)
		}
		first, last := f.bounds(t, Accounts)
		f.checkAccountsAt(t, want, first, last, accounts)

		// LoadUntil from a version on: the events Load reads, up to upto.
		err := f.db.View(ctx, func(tx *sql.Tx) error {
			for id := range int64(accounts) {
				id := Accounts.ID(id + 1)
				var all []events.Record[int64, AccountEvent]
				for r, err := range events.Load(ctx, f.st, tx, id, 0) {
					if err != nil {
						return err
					}
					all = append(all, r)
				}
				for upto := first; upto <= last; upto += 1 + rnd.Int64N(5) {
					var after int64
					if len(all) > 0 {
						after = all[0].Version - 1 + rnd.Int64N(int64(len(all))+1)
					}
					var got, want []int64
					for r, err := range events.LoadUntil(ctx, f.st, tx, id, after, upto) {
						if err != nil {
							return err
						}
						got = append(got, r.Position)
					}
					for _, r := range all {
						if r.Version > after && r.Position <= upto {
							want = append(want, r.Position)
						}
					}
					if !slices.Equal(got, want) {
						return fmt.Errorf("account %v after %d up to %d: %v, want %v", id, after, upto, got, want)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	if from == 0 {
		t.Error("nothing compacted")
	}
}
