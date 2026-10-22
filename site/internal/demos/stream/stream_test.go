package stream

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/demo/demotest"
)

var me = demo.Viewer{Session: "s1", Tab: "t1"}

// The payload changes exactly as many cells in each step as its churn
// says, marks exactly those, and comes round to every cell.
func TestPayload(t *testing.T) {
	want := map[Size][4]int{Small: {0, 1, 4, 24}, Medium: {0, 2, 16, 96}, Large: {0, 8, 64, 384}}
	for _, s := range Sizes {
		for i, c := range Churns {
			if got := Changes(s, c); got != want[s][i] {
				t.Errorf("%s %s: %d cells a step, want %d", s, c, got, want[s][i])
			}
		}
	}
	const step = 7_000_000_000 // about the step of 2026
	for _, s := range Sizes {
		for _, c := range Churns[1:] {
			k := Changes(s, c)
			n := s.Rows() * cols
			changed := map[int]int{}
			prev := payload(s, k, step-1, true)
			for i := range n / k {
				cur := payload(s, k, step+int64(i), true)
				marked := 0
				for r := range cur.Rows {
					for col := range cols {
						cell, was := cur.Rows[r].Cells[col], prev.Rows[r].Cells[col]
						if cell.New {
							marked++
							changed[r*cols+col]++
						} else if cell.Text != was.Text {
							t.Fatalf("%s %s: a cell changed unmarked", s, c)
						}
					}
				}
				if marked != k {
					t.Fatalf("%s %s: %d cells marked, want %d", s, c, marked, k)
				}
				prev = cur
			}
			if len(changed) != n {
				t.Errorf("%s %s: %d of %d cells changed in a round", s, c, len(changed), n)
			}
		}
	}
	a, b := payload(Large, 8, step, true), payload(Large, 8, step, true)
	if !slices.Equal(a.Rows, b.Rows) || a.Rows[0].Name != "node-001" || len(a.Rows) != 256 {
		t.Error("the payload is not a function of its step")
	}
	still := payloadFor(TabState{Churn: Still, Frozen: step, From: Busy}, step+100)
	if still.Changes != 0 || !slices.Equal(still.Rows, payload(Large, Changes(Large, Busy), step, false).Rows) {
		t.Error("a still table does not show the step it froze at")
	}
	for _, r := range still.Rows {
		for _, c := range r.Cells {
			if c.New {
				t.Fatal("a still table marks a cell")
			}
		}
	}
}

// The memo keeps two steps of each payload at most.
func TestMemo(t *testing.T) {
	ctx := context.Background()
	m := &memo{m: map[key][]byte{}}
	for step := range int64(10) {
		for _, c := range Churns[1:] {
			if _, err := m.get(ctx, key{Small, c, step}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(m.m) != 6 {
		t.Errorf("the memo holds %d frames", len(m.m))
	}
	b, _ := m.get(ctx, key{Small, Calm, 9})
	if !strings.Contains(string(b), `id="stream-payload"`) || !strings.Contains(string(b), `id="stream-payload-note"`) || strings.Count(string(b), "data-new") != 1 {
		t.Errorf("a frame of the small table:\n%s", b)
	}
}

func TestSetPayload(t *testing.T) {
	e := demotest.Open(t, Demo)
	at := time.Unix(1_800_000_000, 0)
	read := func() (st TabState) {
		t.Helper()
		e.View(t, func(ctx context.Context, tx *sql.Tx) error { return demo.LoadTab(ctx, tx, Slug, me, &st) })
		return st
	}
	e.Do(t, SetPayload{Viewer: me, Op: "op1", Size: Small, At: at})
	if st := read(); st.size() != Small || st.churn() != DefaultChurn || st.Op != "op1" {
		t.Fatalf("a smaller table: %+v", st)
	}
	if got := e.Touched(); !slices.Equal(got, []string{demo.TabTopic(Slug, me.Tab)}) {
		t.Errorf("a change touched %v", got)
	}
	e.Do(t, SetPayload{Viewer: me, Op: "op2", Churn: Busy, At: at})
	e.Do(t, SetPayload{Viewer: me, Op: "op3", Churn: Still, At: at})
	if st := read(); st.churn() != Still || st.Frozen != StepAt(at) || st.From != Busy || st.size() != Small {
		t.Fatalf("still: %+v", st)
	}
	// Still again keeps the step it froze at.
	e.Do(t, SetPayload{Viewer: me, Op: "op4", Churn: Still, At: at.Add(time.Minute)})
	if st := read(); st.Frozen != StepAt(at) || st.From != Busy {
		t.Fatalf("still twice: %+v", st)
	}
	for name, c := range map[string]SetPayload{
		"no op":     {Viewer: me, Size: Large},
		"nothing":   {Viewer: me, Op: "x"},
		"bad size":  {Viewer: me, Op: "x", Size: "huge"},
		"bad churn": {Viewer: me, Op: "x", Churn: "wild"},
	} {
		if err := e.W.Do(context.Background(), c); !errors.Is(err, writer.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The views a tab watches: the report, its choices with the op that
// changed them, and its table, which renders nothing while it has nothing
// new.
func TestViews(t *testing.T) {
	e := demotest.Open(t, Demo)
	vs := pageViews(me)
	keys := []string{vs[0].Key, vs[1].Key, vs[2].Key}
	if !slices.Equal(keys, []string{"stream:report", "tab:stream:t1", "stream:table:t1"}) || vs[0].Every != time.Second || vs[2].Every != Step {
		t.Fatalf("views: %v", keys)
	}
	if ev := embedViews(me); len(ev) != 1 || ev[0].Key != "stream:report:embed" {
		t.Fatalf("the slot's views: %+v", ev)
	}
	render := func(v int) any {
		t.Helper()
		var f any
		e.View(t, func(ctx context.Context, tx *sql.Tx) (err error) {
			f, err = vs[v].Render(ctx, tx)
			return err
		})
		return f
	}
	if p, ok := render(0).(demo.Probe); !ok || p.(probe).embed {
		t.Fatalf("the report's frame: %T", p)
	}
	if b := string(render(1).([]byte)); !strings.Contains(b, `"_stream":{"size":"large","churn":"calm","ack":"","synced":true}`) {
		t.Errorf("a new tab's choices:\n%s", b)
	}
	if b := render(2).([]byte); strings.Count(string(b), "<tr>") != 257 {
		t.Errorf("a new tab's table has %d rows", strings.Count(string(b), "<tr>")-1)
	}
	e.Do(t, SetPayload{Viewer: me, Op: "op1", Size: Small, Churn: Still, At: time.Now()})
	if b := string(render(1).([]byte)); !strings.Contains(b, `{"size":"small","churn":"still","ack":"op1","synced":true}`) {
		t.Errorf("the choices after a change:\n%s", b)
	}
	b, ok := render(2).([]byte)
	if !ok || strings.Count(string(b), "<tr>") != 17 || strings.Contains(string(b), "data-new") || !strings.Contains(string(b), "Still: nothing changes, so your stream sends nothing.") {
		t.Fatalf("a small still table:\n%s", b)
	}
	if f := render(2); f != nil {
		t.Errorf("a still table rendered again: %T", f)
	}
	var page strings.Builder
	e.View(t, func(ctx context.Context, tx *sql.Tx) error {
		c, err := renderPage(ctx, tx, me)
		if err != nil {
			return err
		}
		return c.Render(ctx, &page)
	})
	if !strings.Contains(page.String(), `size: &#39;small&#39;, churn: &#39;still&#39;`) || strings.Count(page.String(), "<tr>") != 17 {
		t.Errorf("the page of a small still table: %d rows\n%.600s", strings.Count(page.String(), "<tr>"), page.String())
	}
}

// capture keeps the reports of a probe.
type capture struct {
	p   probe
	got [][]byte
}

func (c *capture) Report(st *sse.Stream, m *demo.Meter, frames int) []byte {
	b := c.p.Report(st, m, frames)
	if b != nil {
		c.got = append(c.got, b)
	}
	return b
}

func (c *capture) page(t *testing.T, i int) PageReport {
	t.Helper()
	var r pageReport
	body := strings.TrimSuffix(strings.TrimPrefix(string(c.got[i]), "event: datastar-patch-signals\ndata: signals "), "\n\n")
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("%v: %s", err, c.got[i])
	}
	return r.S
}

// A report reads the stream: its coding and tier, the bytes so far, the
// last frame, and its budget. The page's probe reports with every frame,
// the slot's once a second, and a stream that fell quiet gets one report
// of its last frame alone.
func TestReport(t *testing.T) {
	budget := sse.NewBudget(sse.BudgetOptions{Limit: 64 << 20})
	r := httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)
	r.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	st, err := sse.Open(httptest.NewRecorder(), r, sse.Options{Budget: budget})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var ps demo.Probes
	c := &capture{}
	ps.Keep("stream:report", c)
	frame, err := frames.get(context.Background(), key{Large, Calm, 7_000_000_000})
	if err != nil {
		t.Fatal(err)
	}
	write := func(events ...[]byte) {
		t.Helper()
		if err := ps.Write(st, events); err != nil {
			t.Fatal(err)
		}
	}
	write(frame)
	write(frame)
	if len(c.got) != 2 {
		t.Fatalf("%d reports for two frames", len(c.got))
	}
	first, second := c.page(t, 0), c.page(t, 1)
	// The first report describes the stream before its first frame, which
	// picks its tier.
	if first.N != 0 || first.Coding != "zstd" || first.Tier != "zstd asleep" || first.Heap != "none" || first.Events != "0 B" || first.Streams != 1 || first.Reserved != "32 KiB" || first.Limit != "64 MiB" {
		t.Errorf("the first report: %+v", first)
	}
	if second.N != 1 || second.FE == "" || second.FW <= 0 || second.FR < 10 || second.Tier != "zstd 2^19" || second.Heap != "1.7 MiB" || second.Tiers != "zstd 2^19 (1)" || second.Reserved != "1.73 MiB" || second.Budget != 3 {
		t.Errorf("the second report: %+v", second)
	}
	// The same frame again costs little: its ratio is far above the first's.
	write(frame)
	if third := c.page(t, 2); third.FR < 10*20 || third.FW >= second.FW || third.Ratio == "" {
		t.Errorf("a repeated frame: %+v after %+v", third, second)
	}

	// Quiet: a tick of the probe alone reports the last frame, once.
	write()
	if len(c.got) != 3 {
		t.Fatal("a report alone before the stream was quiet")
	}
	time.Sleep(quiet)
	write()
	write()
	if len(c.got) != 4 || c.page(t, 3).N != 3 {
		t.Fatalf("quiet: %d reports", len(c.got))
	}

	// The slot's probe: once a second at most.
	var slot demo.Probes
	e := &capture{p: probe{embed: true}}
	slot.Keep("stream:report:embed", e)
	for range 3 {
		if err := slot.Write(st, [][]byte{frame}); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.got) != 1 || !strings.Contains(string(e.got[0]), `"coding":"zstd","tier":"zstd 2^19"`) {
		t.Errorf("the slot reported %d times in three quick frames: %q", len(e.got), e.got)
	}
}

func TestFormat(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 87: "87 B", 9_876: "9,876 B", 25_412: "25.4 KB", 312_400: "312 KB", 1_240_000: "1.24 MB", 12_300_000: "12.3 MB", 2_000_000_000: "2 GB"} {
		if got := byteText(n); got != want {
			t.Errorf("byteText(%d) = %s, want %s", n, got, want)
		}
	}
	for n, want := range map[int64]string{0: "none", 848 << 10: "848 KiB", 1696 << 10: "1.66 MiB", 96 << 20: "96 MiB", 11_600_000: "11.1 MiB"} {
		if got := mibText(n); got != want {
			t.Errorf("mibText(%d) = %s, want %s", n, got, want)
		}
	}
	for c, want := range map[[2]int64]string{{0, 0}: "1:1", {100, 100}: "1:1", {340, 100}: "3.4:1", {25_412, 87}: "292:1", {1_000_000, 10}: "100,000:1"} {
		if got := ratioText(c[0], c[1]); got != want {
			t.Errorf("ratioText(%v) = %s, want %s", c, got, want)
		}
	}
	if got := tiersText(map[string]int{"zstd 2^19": 5, "zstd asleep": 2, "identity": 2, "gzip": 1, "br q4 2^18": 1}); got != "zstd 2^19 (5), uncompressed (2), zstd asleep (2), 2 more" {
		t.Errorf("tiers: %s", got)
	}
}

// TestQuiet runs the reports through lith's hub, with a heartbeat after
// 100 ms of silence and hibernation after 300 ms: a still table falls
// quiet and its last frame gets a report alone; the frame that wakes the
// stream reports the heartbeats sent meanwhile and the encoder it gave
// back; the next report, the encoder it took again and what the frame
// without history cost. (On the site the report alone goes out a second
// after the last frame, long before the first heartbeat at 20 s.)
func TestQuiet(t *testing.T) {
	e := demotest.Open(t, Demo)
	e.Do(t, SetPayload{Viewer: me, Op: "op1", Size: Small, Churn: Still, At: time.Now()})
	hub := live.New(live.Options{Read: e.DB.R, Workers: 1, Heartbeat: 100 * time.Millisecond, Hibernate: 300 * time.Millisecond})
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { hub.Run(ctx); close(done) }()
	defer func() { stop(); <-done }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ps demo.Probes
		hub.Serve(w, r, live.Spec{ID: me.Tab, Views: pageViews(me), WriteTick: func(st *sse.Stream, frames []live.Frame) error {
			var events [][]byte
			for _, f := range frames {
				switch v := f.Value.(type) {
				case []byte:
					events = append(events, v)
				case demo.Probe:
					ps.Keep(f.Key, v)
				}
			}
			return ps.Write(st, events)
		}})
	}))
	defer srv.Close()
	rctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(rctx, "GET", srv.URL, nil)
	req.Header.Set("Accept-Encoding", "zstd")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reports := make(chan PageReport, 16)
	go func() {
		defer close(reports)
		dec, err := zstd.NewReader(res.Body)
		if err != nil {
			return
		}
		defer dec.Close()
		lines := bufio.NewScanner(dec)
		lines.Buffer(nil, 1<<20)
		for lines.Scan() {
			body, ok := strings.CutPrefix(lines.Text(), "data: signals ")
			if !ok || !strings.Contains(body, `"n":`) {
				continue
			}
			var r pageReport
			if json.Unmarshal([]byte(body), &r) == nil {
				reports <- r.S
			}
		}
	}()
	defer func() {
		cancel()
		res.Body.Close()
		for range reports {
		}
	}()
	next := func(what string) PageReport {
		t.Helper()
		select {
		case r := <-reports:
			return r
		case <-time.After(3 * time.Second):
			t.Fatalf("no report: %s", what)
		}
		return PageReport{}
	}
	if r := next("the first frame"); r.N != 0 || r.Coding != "zstd" {
		t.Fatalf("the first report: %+v", r)
	}
	if r := next("the quiet stream's last frame"); r.N != 1 || r.FW <= 0 {
		t.Fatalf("the report alone: %+v", r)
	}
	time.Sleep(time.Second) // heartbeats, then hibernation

	e.Do(t, SetPayload{Viewer: me, Op: "op2", Churn: Calm, At: time.Now()})
	hub.Changed([]string{demo.TabTopic(Slug, me.Tab)})
	wake := next("the frame that wakes the stream")
	if wake.N != 1 || wake.Beats < 3 || wake.Tier != "zstd asleep" || wake.Heap != "none" {
		t.Fatalf("waking: %+v", wake)
	}
	awake := next("the frame after")
	if awake.N != 2 || awake.Tier == "zstd asleep" || awake.Heap == "none" {
		t.Fatalf("awake: %+v", awake)
	}
	if later := next("a frame with history"); later.FW*2 > awake.FW {
		t.Errorf("a frame with history took %d bytes, the one that woke the stream %d", later.FW, awake.FW)
	}
}
