package writerrace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/demo/demotest"
)

var me = demo.Viewer{Session: "s1", Tab: "t1"}

func startCmd(v demo.Viewer, op string, p int, sync string, at time.Time) Start {
	return Start{Viewer: v, Op: op, Settings: Settings{p, sync}, At: at}
}

func tab(t *testing.T, e *demotest.Env, v demo.Viewer) (o outcome) {
	t.Helper()
	e.View(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		o, err = loadOutcome(ctx, tx, v)
		return err
	})
	return o
}

func states(t *testing.T, e *demotest.Env) (out []string) {
	t.Helper()
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, state FROM writer_race_races ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var s string
			rows.Scan(&id, &s)
			out = append(out, fmt.Sprintf("%d:%s", id, s))
		}
		return rows.Err()
	})
	return out
}

// TestStart puts races in line, one per session, and refuses more than
// Waiting: each refusal goes to the tab that asked, next to its button.
func TestStart(t *testing.T) {
	e := demotest.Open(t, Demo)
	at := time.UnixMilli(1_800_000_000_000)
	woke := 0
	cmd := startCmd(me, "op1", 200, "FULL", at)
	cmd.Joined = func() { woke++ }
	e.Do(t, cmd)
	o := tab(t, e, me)
	if o.State.Op != "op1" || o.State.Refusal != "" || o.Race.ID != 1 || o.Race.State != "queued" || o.Place != 1 || o.Race.Settings != (Settings{200, "FULL"}) {
		t.Fatalf("in line: %+v", o)
	}
	if woke != 1 {
		t.Errorf("the runner was woken %d times", woke)
	}
	if got := e.Touched(); !slices.Equal(got, []string{"line:writer-race", "tab:writer-race:t1"}) {
		t.Errorf("a start touched %v", got)
	}
	if !strings.Contains(said(o), "Race 1 is yours: next on the track.") {
		t.Errorf("the tab hears %q", said(o))
	}

	// The same session again, from another tab: refused, and told which
	// race is its own.
	other := demo.Viewer{Session: "s1", Tab: "t2"}
	again := startCmd(other, "op2", 50, "NORMAL", at)
	again.Joined = func() { woke++ }
	e.Do(t, again)
	if o := tab(t, e, other); o.Race.ID != 1 || !o.State.Again || said(o) != "Race 1 is yours: next on the track. One race at a time for each visitor." {
		t.Fatalf("a second race of a session: %+v, %q", o, said(o))
	}
	if got := e.Touched(); !slices.Equal(got, []string{"tab:writer-race:t2"}) {
		t.Errorf("a refusal touched %v", got)
	}

	// Others fill the line up to Waiting; the next one is refused.
	for i := 2; i <= Waiting; i++ {
		e.Do(t, startCmd(demo.Viewer{Session: fmt.Sprint("s", i), Tab: "t"}, "op", 1, "NORMAL", at))
	}
	late := demo.Viewer{Session: "late", Tab: "t9"}
	e.Do(t, startCmd(late, "op", 1, "NORMAL", at))
	if o := tab(t, e, late); o.Race.ID != 0 || !strings.Contains(o.State.Refusal, fmt.Sprintf("%d races wait already", Waiting)) {
		t.Fatalf("a full line: %+v", o)
	}
	if o := tab(t, e, demo.Viewer{Session: "s4", Tab: "t"}); o.Place != 4 || !strings.Contains(said(o), "4th in line") {
		t.Errorf("the fourth in line: %+v, %q", o, said(o))
	}
	if woke != 1 {
		t.Errorf("refusals woke the runner: %d", woke)
	}

	for name, bad := range map[string]Start{
		"no tab":    startCmd(demo.Viewer{Session: "s"}, "op", 1, "FULL", at),
		"no op":     startCmd(me, "", 1, "FULL", at),
		"producers": startCmd(me, "op", 100, "FULL", at),
		"sync":      startCmd(me, "op", 1, "OFF", at),
	} {
		if err := e.W.Do(context.Background(), bad); !errors.Is(err, writer.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestLifecycle walks a race through its states on the site's writer: it
// waits Dwell in line, runs, takes samples, and ends with its numbers.
func TestLifecycle(t *testing.T) {
	e := demotest.Open(t, Demo)
	t0 := time.UnixMilli(1_800_000_000_000)
	e.Do(t, startCmd(me, "op1", 50, "NORMAL", t0))
	e.Touched()
	claimAt := func(now time.Time) next {
		t.Helper()
		n, err := e.W.DoValue(context.Background(), claim(now, Dwell))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := claimAt(t0.Add(300 * time.Millisecond)); n.Race.ID != 0 || n.Wait != Dwell-300*time.Millisecond {
		t.Fatalf("a race in line for 300 ms: %+v", n)
	}
	n := claimAt(t0.Add(Dwell))
	if n.Race.ID != 1 || n.Race.Settings != (Settings{50, "NORMAL"}) {
		t.Fatalf("claimed %+v", n)
	}
	if got := e.Touched(); !slices.Equal(got, []string{"line:writer-race", "live:writer-race", "writer-race"}) {
		t.Errorf("a claim touched %v", got)
	}
	if o := tab(t, e, me); o.Race.State != "running" || said(o) != "Race 1 is yours: on the track now." {
		t.Errorf("the tab of the running race: %+v", o)
	}

	s := Sample{At: 50 * time.Millisecond, Commands: 1200, Batches: 48, Queue: 22, MaxBatch: 30, P99: 900 * time.Microsecond, Sizes: Sizes{0, 0, 0, 2, 40, 6}}
	e.Do(t, Record{Race: 1, Sample: s})
	s2 := s
	s2.At, s2.Commands, s2.Batches = 100*time.Millisecond, 2500, 100
	e.Do(t, Record{Race: 1, Sample: s2})
	if got := e.Touched(); !slices.Equal(got, []string{"live:writer-race"}) {
		t.Errorf("a sample touched %v", got)
	}
	var tr track
	e.View(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		tr, err = loadTrack(ctx, tx)
		return err
	})
	if !tr.Running || tr.Race.Last.Commands != 2500 || tr.Race.Last.Sizes != s.Sizes || tr.Race.Last.P99 != s.P99 || len(tr.Bars) != 8 {
		t.Fatalf("the track: %+v", tr)
	}
	if tr.Rate != 25_000 || tr.PerTx != 25 {
		t.Errorf("the track's numbers now: %v a second, %v per transaction", tr.Rate, tr.PerTx)
	}

	last := s2
	last.At, last.Commands, last.Batches, last.Queue = 3010*time.Millisecond, 150_500, 6020, 0
	e.Do(t, Finish{Race: 1, Last: last, EndedAt: t0.Add(5 * time.Second)})
	if got := e.Touched(); !slices.Equal(got, []string{"live:writer-race", "results:writer-race", "writer-race"}) {
		t.Errorf("a finish touched %v", got)
	}
	// A sample that comes after the end changes nothing.
	e.Do(t, Record{Race: 1, Sample: s})
	if got := e.Touched(); len(got) != 0 {
		t.Errorf("a late sample touched %v", got)
	}
	o := tab(t, e, me)
	if o.Race.State != "done" || o.Race.Last.Commands != 150_500 || said(o) != "Race 1 is yours: 50,000 commands a second, 25 to a transaction." {
		t.Fatalf("done: %+v, %q", o.Race, said(o))
	}
	if n := claimAt(t0.Add(time.Minute)); n.Race.ID != 0 || n.Wait != 0 {
		t.Errorf("an empty line: %+v", n)
	}
}

// TestInterrupted ends the races a process left on the track: when the
// site starts (recovered), and when the runner finds one.
func TestInterrupted(t *testing.T) {
	e := demotest.Open(t, Demo)
	t0 := time.UnixMilli(1_800_000_000_000)
	e.Do(t, startCmd(me, "op1", 1, "FULL", t0))
	e.Do(t, startCmd(demo.Viewer{Session: "s2", Tab: "t2"}, "op1", 1, "FULL", t0))
	if n, err := e.W.DoValue(context.Background(), claim(t0.Add(Dwell), Dwell)); err != nil || n.Race.ID != 1 {
		t.Fatalf("claimed %+v, %v", n, err)
	}
	e.Touched()
	woke := 0
	e.Do(t, recovered(t0.Add(time.Minute), func() { woke++ }))
	if got := states(t, e); !slices.Equal(got, []string{"1:interrupted", "2:queued"}) || woke != 1 {
		t.Fatalf("after a restart: %v, woken %d", got, woke)
	}
	if got := e.Touched(); !slices.Equal(got, []string{"live:writer-race", "results:writer-race", "writer-race"}) {
		t.Errorf("recovering touched %v", got)
	}
	if o := tab(t, e, me); said(o) != "Race 1 is yours: the server restarted during it." {
		t.Errorf("the tab hears %q", said(o))
	}
	// The runner's claim does the same for a race left running.
	e.Do(t, writer.Func(func(tx *writer.Tx) error {
		_, err := tx.Exec(`UPDATE writer_race_races SET state = 'running' WHERE id = 2`)
		return err
	}))
	if n, err := e.W.DoValue(context.Background(), claim(t0.Add(time.Minute), Dwell)); err != nil || n.Race.ID != 0 {
		t.Fatalf("claimed %+v, %v", n, err)
	}
	if got := states(t, e); !slices.Equal(got, []string{"1:interrupted", "2:interrupted"}) {
		t.Fatalf("after the claim: %v", got)
	}
	e.Do(t, recovered(t0.Add(time.Hour), func() { woke++ }))
	if woke != 1 {
		t.Errorf("nothing waits, yet the runner was woken")
	}
}

func TestSweep(t *testing.T) {
	e := demotest.Open(t, Demo)
	now := time.UnixMilli(1_800_000_000_000)
	e.Do(t, writer.Func(func(tx *writer.Tx) error {
		for range Kept + 5 {
			res, err := tx.Exec(`INSERT INTO writer_race_races (sid, producers, sync, state, queued_at) VALUES ('s', 1, 'FULL', 'done', ?)`, now.UnixMilli())
			if err != nil {
				return err
			}
			id, _ := res.LastInsertId()
			for at := range 3 {
				if _, err := tx.Exec(`INSERT INTO writer_race_samples (race_id, at_ms, commands, batches, queue, p99_us) VALUES (?, ?, 1, 1, 0, 1)`, id, at*50); err != nil {
					return err
				}
			}
		}
		// One that waited too long, one that just joined.
		if _, err := tx.Exec(`INSERT INTO writer_race_races (sid, producers, sync, state, queued_at) VALUES ('a', 1, 'FULL', 'queued', ?), ('b', 1, 'FULL', 'queued', ?)`,
			now.Add(-Stale-time.Second).UnixMilli(), now.UnixMilli()); err != nil {
			return err
		}
		return nil
	}))
	e.Touched()
	e.Do(t, writer.Func(func(tx *writer.Tx) error { return sweep(tx, now) }))
	var races, charted int
	var states []string
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM writer_race_races`).Scan(&races); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(DISTINCT race_id) FROM writer_race_samples`).Scan(&charted); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT state FROM writer_race_races ORDER BY id DESC LIMIT 2`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			rows.Scan(&s)
			states = append(states, s)
		}
		return nil
	})
	if races != Kept || charted != Charted-2 || !slices.Equal(states, []string{"queued", "dropped"}) {
		t.Errorf("after the sweep: %d races, %d with samples, the newest %v", races, charted, states)
	}
	// The dropped race leaves the line; the races deleted are the oldest,
	// which no view shows.
	if got := e.Touched(); !slices.Equal(got, []string{"line:writer-race", "writer-race"}) {
		t.Errorf("the sweep touched %v", got)
	}
	e.Do(t, writer.Func(func(tx *writer.Tx) error { return sweep(tx, now) }))
	if got := e.Touched(); len(got) != 0 {
		t.Errorf("a sweep that deleted nothing touched %v", got)
	}
}

// TestViews renders the views as a tab's stream gets them.
func TestViews(t *testing.T) {
	e := demotest.Open(t, Demo)
	t0 := time.Now()
	e.Do(t, startCmd(me, "op1", 200, "NORMAL", t0.Add(-time.Minute)))
	e.Do(t, startCmd(demo.Viewer{Session: "s2", Tab: "t2"}, "op9", 1, "FULL", t0.Add(-time.Minute)))
	if n, err := e.W.DoValue(context.Background(), claim(t0, Dwell)); err != nil || n.Race.ID != 1 {
		t.Fatalf("claim: %+v %v", n, err)
	}
	for i := 1; i <= 3; i++ {
		e.Do(t, Record{Race: 1, Sample: Sample{At: time.Duration(i) * Every, Commands: int64(i) * 4000, Batches: int64(i) * 40, Queue: 90, P99: 2 * time.Millisecond, Sizes: Sizes{0, 0, 0, 0, 0, 0, int64(i) * 30, int64(i) * 10}}})
	}
	render := func(v live.View) (out string) {
		t.Helper()
		e.View(t, func(ctx context.Context, tx *sql.Tx) error {
			f, err := v.Render(ctx, tx)
			switch f := f.(type) {
			case []byte:
				out = string(f)
			case demo.Frame:
				if strings.Contains(string(f.First), "useViewTransition") || !strings.Contains(string(f.Next), "useViewTransition true") {
					t.Errorf("an animated view's frames:\n%s", f.First)
				}
				out = string(f.Next)
			}
			return err
		})
		return out
	}
	vs := pageViews(me)
	keys := []string{}
	for _, v := range vs {
		keys = append(keys, v.Key)
	}
	if !slices.Equal(keys, []string{"writer-race:track", "writer-race:line", "writer-race:results", "tab:writer-race:t1"}) {
		t.Fatalf("views: %v", keys)
	}
	track := render(vs[0])
	for _, want := range []string{`id="writer-race-track"`, `data-state="running"`, `Race 1`, `200 producers`, `80,000`, `<sb-gauge value="100.0"`, `<sb-meter label="Queue" value="90" max="200"`, `bench, this server: 80,530`, `--h: 0.333`, `--h: 1.000`, `<sb-sparkline values="[2.00,2.00,2.00]"`} {
		if !strings.Contains(track, want) {
			t.Errorf("the track lacks %q:\n%s", want, track)
		}
	}
	if line := render(vs[1]); !strings.Contains(line, `id="writer-race-wait-1" class="wr-wait is-on"`) || !strings.Contains(line, `id="writer-race-wait-2" class="wr-wait"`) || strings.Contains(line, "useViewTransition") {
		t.Errorf("the line:\n%s", line)
	}
	if results := render(vs[2]); !strings.Contains(results, "Every race that ends lands here") {
		t.Errorf("the results:\n%s", results)
	}
	tabFrame := render(vs[3])
	if !strings.Contains(tabFrame, "Race 1 is yours: on the track now.") || !strings.Contains(tabFrame, `"_writerRace":{"ack":"op1","mine":1,"active":true}`) {
		t.Errorf("the tab's view:\n%s", tabFrame)
	}

	e.Do(t, Finish{Race: 1, Last: Sample{At: 3 * time.Second, Commands: 240_000, Batches: 2000, P99: 3 * time.Millisecond, Sizes: Sizes{0, 0, 0, 0, 0, 0, 1500, 500}}, EndedAt: time.Now()})
	track = render(vs[0])
	for _, want := range []string{`data-state="done"`, `class="wr-result" data-fresh`, "240,000", "Finished in 3.0 s", "80,000"} {
		if !strings.Contains(track, want) {
			t.Errorf("the finished track lacks %q:\n%s", want, track)
		}
	}
	results := render(vs[2])
	for _, want := range []string{`id="writer-race-result-1"`, `data-class:is-mine="$_writerRace.mine === 1"`, "data-fresh", "80,000", "200, NORMAL"} {
		if !strings.Contains(results, want) {
			t.Errorf("the results lack %q:\n%s", want, results)
		}
	}
	if f := render(vs[3]); !strings.Contains(f, `"active":false`) {
		t.Errorf("the tab's view after the race:\n%s", f)
	}
	embed := render(embedViews(me)[0])
	if !strings.Contains(embed, `id="writer-race-embed"`) || !strings.Contains(embed, "80,000") {
		t.Errorf("the embed:\n%s", embed)
	}

	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		page, err := renderPage(ctx, tx, me)
		if err != nil {
			return err
		}
		var html strings.Builder
		page.Render(ctx, &html)
		for _, want := range []string{`data-bind="_writerRace.producers"`, `value="200" data-bind="_writerRace.producers" checked`, `id="writer-race-outcome"`, `id="writer-race-line"`, "240,000"} {
			if !strings.Contains(html.String(), want) {
				t.Errorf("the page lacks %q", want)
			}
		}
		return nil
	})
}

func TestRates(t *testing.T) {
	pts := []Point{
		{At: 50 * time.Millisecond, Commands: 100},
		{At: 100 * time.Millisecond, Commands: 300},
		{At: 120 * time.Millisecond, Commands: 340}, // too close: joins the next
		{At: 150 * time.Millisecond, Commands: 400},
		{At: 152 * time.Millisecond, Commands: 404}, // the last, close: widens the one before
	}
	got := rates(pts)
	want := []step{{50 * time.Millisecond, 2000}, {100 * time.Millisecond, 4000}, {152 * time.Millisecond, 104 / 0.052}}
	if len(got) != len(want) {
		t.Fatalf("rates %v", got)
	}
	for i := range got {
		if got[i].At != want[i].At || got[i].Rate-want[i].Rate > 1e-6 || want[i].Rate-got[i].Rate > 1e-6 {
			t.Errorf("step %d: %v, want %v", i, got[i], want[i])
		}
	}
	for v, w := range map[float64]float64{0: 1, 0.7: 0.8, 13_260: 15_000, 66_688: 80_000, 74_000: 80_000, 100_000: 100_000, 101_000: 120_000} {
		if got := nice(v); got != w {
			t.Errorf("nice(%v) = %v, want %v", v, got, w)
		}
	}
	for n, w := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 66688: "66,688", 1234567: "1,234,567", -5000: "-5,000"} {
		if got := group(n); got != w {
			t.Errorf("group(%d) = %q", n, got)
		}
	}
	for n, w := range map[int]string{2: "2nd", 3: "3rd", 4: "4th", 11: "11th", 21: "21st"} {
		if got := ordinal(n); got != w {
			t.Errorf("ordinal(%d) = %q", n, got)
		}
	}
}

func testEngine(t *testing.T) *Engine {
	return &Engine{Dir: t.TempDir(), Duration: 300 * time.Millisecond, Every: 25 * time.Millisecond, Log: slog.New(slog.DiscardHandler)}
}

// TestEngine runs a short race on a file of its own: the samples add up,
// and the file is gone afterwards.
func TestEngine(t *testing.T) {
	e := testEngine(t)
	var samples []Sample
	last, err := e.Run(context.Background(), 7, Settings{50, "NORMAL"}, func(s Sample) { samples = append(samples, s) })
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) < 5 || samples[len(samples)-1] != last {
		t.Fatalf("%d samples, the last %+v of %+v", len(samples), samples[len(samples)-1], last)
	}
	var sum int64
	for _, n := range last.Sizes {
		sum += n
	}
	if last.Commands == 0 || last.Failures != 0 || sum != last.Batches || last.MaxBatch > 50 || last.At < e.Duration {
		t.Fatalf("the race: %+v", last)
	}
	for i := 1; i < len(samples); i++ {
		if samples[i].Commands < samples[i-1].Commands || samples[i].At < samples[i-1].At {
			t.Fatalf("samples go back: %+v then %+v", samples[i-1], samples[i])
		}
	}
	if files, _ := filepath.Glob(filepath.Join(e.Dir, "*")); len(files) != 0 {
		t.Errorf("left behind: %v", files)
	}

	// A race whose context ends stops early, and leaves nothing either.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	e.Duration = 10 * time.Second
	t0 := time.Now()
	if _, err := e.Run(ctx, 8, Settings{200, "FULL"}, func(Sample) {}); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled race: %v", err)
	}
	if time.Since(t0) > 2*time.Second {
		t.Errorf("a cancelled race took %v", time.Since(t0))
	}
	if files, _ := filepath.Glob(filepath.Join(e.Dir, "*")); len(files) != 0 {
		t.Errorf("left behind: %v", files)
	}

	// Files that a process left behind go when the next one starts.
	for _, f := range []string{"writer-race-3.db", "writer-race-3.db-wal", "site.db"} {
		os.WriteFile(filepath.Join(e.Dir, f), nil, 0o600)
	}
	e.Clean()
	if files, _ := filepath.Glob(filepath.Join(e.Dir, "*")); len(files) != 1 || filepath.Base(files[0]) != "site.db" {
		t.Errorf("after Clean: %v", files)
	}
}

// TestRunner runs races through the line as the site does: one at a time,
// each one after the other ended, every file removed.
func TestRunner(t *testing.T) {
	env := demotest.Open(t, Demo)
	eng := testEngine(t)
	r := &runner{w: env.W, e: eng, log: eng.Log, pause: 20 * time.Millisecond, dwell: 30 * time.Millisecond}
	var files sync.Map
	eng.Log = slog.New(slog.DiscardHandler)
	for i, s := range []Settings{{200, "NORMAL"}, {1, "FULL"}, {50, "NORMAL"}} {
		cmd := startCmd(demo.Viewer{Session: fmt.Sprint("s", i), Tab: "t"}, "op", s.Producers, s.Sync, time.Now())
		cmd.Joined = r.wake
		env.Do(t, cmd)
	}
	deadline := time.Now().Add(20 * time.Second)
	for !r.idle() || len(states(t, env)) < 3 || slices.ContainsFunc(states(t, env), func(s string) bool { return !strings.HasSuffix(s, ":done") }) {
		if time.Now().After(deadline) {
			t.Fatalf("races %v, runner idle %v", states(t, env), r.idle())
		}
		if f, _ := filepath.Glob(filepath.Join(eng.Dir, "writer-race-*.db")); len(f) > 1 {
			t.Fatalf("two races at once: %v", f)
		}
		for _, f := range must(filepath.Glob(filepath.Join(eng.Dir, "*.db"))) {
			files.Store(f, true)
		}
		time.Sleep(5 * time.Millisecond)
	}
	var list []Race
	env.View(t, func(ctx context.Context, tx *sql.Tx) (err error) {
		list, err = recent(ctx, tx, 10)
		return err
	})
	if len(list) != 3 {
		t.Fatalf("%d races", len(list))
	}
	slices.Reverse(list)
	for i, r := range list {
		if r.Last.Commands == 0 || r.Last.At < eng.Duration || r.StartedAt.Before(r.QueuedAt) {
			t.Errorf("race %d: %+v", r.ID, r)
		}
		if i > 0 && r.StartedAt.Before(list[i-1].EndedAt) {
			t.Errorf("race %d started at %v, before race %d ended at %v", r.ID, r.StartedAt, list[i-1].ID, list[i-1].EndedAt)
		}
	}
	var n int
	env.View(t, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM writer_race_samples`).Scan(&n)
	})
	if n < 3*int(eng.Duration/eng.Every)-3 {
		t.Errorf("%d samples for three races", n)
	}
	if left := must(filepath.Glob(filepath.Join(eng.Dir, "*"))); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	seen := 0
	files.Range(func(any, any) bool { seen++; return true })
	if seen == 0 {
		t.Error("never saw a race's file")
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// TestDeepLine: a race with Deep races or more behind it runs with Capped
// producers at most, and its row says so; claim reports how many wait
// behind it, which the runner's pause grows with.
func TestDeepLine(t *testing.T) {
	e := demotest.Open(t, Demo)
	t0 := time.UnixMilli(1_800_000_000_000)
	for i := range Deep + 1 {
		e.Do(t, startCmd(demo.Viewer{Session: fmt.Sprint("s", i), Tab: "t"}, "op", 200, "NORMAL", t0))
	}
	claimAt := func() next {
		t.Helper()
		n, err := e.W.DoValue(context.Background(), claim(t0.Add(time.Minute), Dwell))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	n := claimAt()
	if n.Race.ID != 1 || n.Behind != Deep || n.Race.Producers != Capped {
		t.Fatalf("with %d behind: %+v", Deep, n)
	}
	var producers int
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT producers FROM writer_race_races WHERE id = 1`).Scan(&producers)
	})
	if producers != Capped {
		t.Errorf("the row says %d producers", producers)
	}
	e.Do(t, Finish{Race: 1, EndedAt: t0.Add(time.Minute)})
	if n := claimAt(); n.Race.ID != 2 || n.Behind != Deep-1 || n.Race.Producers != 200 {
		t.Fatalf("with %d behind: %+v", Deep-1, n)
	}
}
