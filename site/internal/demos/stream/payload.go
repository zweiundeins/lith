package stream

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/zweiundeins/lith/site/internal/demo"
)

// The payload is a table of made-up server metrics that changes every Step:
// a few of its cells at a time, in an order that visits every cell before
// it comes round again. Its rows are a function of the size, the churn and
// the step, so every tab with the same choices shows the same bytes.

// Step is how often the payload changes: four frames a second.
const Step = 250 * time.Millisecond

// Size is how many rows the table has.
type Size string

const (
	Small  Size = "small"
	Medium Size = "medium"
	Large  Size = "large"
)

// Sizes are the sizes, smallest first.
var Sizes = []Size{Small, Medium, Large}

func (s Size) valid() bool { return s == Small || s == Medium || s == Large }

// Rows is the size's number of rows.
func (s Size) Rows() int {
	switch s {
	case Small:
		return 16
	case Medium:
		return 64
	}
	return 256
}

// Churn is how many of the table's cells change in each step.
type Churn string

const (
	Still Churn = "still"
	Calm  Churn = "calm"
	Busy  Churn = "busy"
	Storm Churn = "storm"
)

// Churns are the churns, calmest first.
var Churns = []Churn{Still, Calm, Busy, Storm}

func (c Churn) valid() bool { return c == Still || c == Calm || c == Busy || c == Storm }

// The payload of a new tab: a large table, a few cells changing at a time.
const (
	DefaultSize  = Large
	DefaultChurn = Calm
)

// columns are the metrics of each row.
var columns = [...]string{"req/s", "p50 ms", "p99 ms", "cpu %", "heap MiB", "errors"}

const cols = len(columns)

// Changes is how many cells change in a step: none when still, else one
// cell in 192, one in 24 or one in 4.
func Changes(s Size, c Churn) int {
	n := s.Rows() * cols
	switch c {
	case Calm:
		return max(1, n/192)
	case Busy:
		return n / 24
	case Storm:
		return n / 4
	}
	return 0
}

// StepAt is the step of the payload at t.
func StepAt(t time.Time) int64 { return t.UnixMilli() / Step.Milliseconds() }

// order holds, for each size, every cell's place in the order of changes:
// a shuffle, the same in every process.
var order = func() map[Size][]int32 {
	m := map[Size][]int32{}
	for i, s := range Sizes {
		n := s.Rows() * cols
		perm := make([]int32, n)
		for j := range perm {
			perm[j] = int32(j)
		}
		x := uint64(i + 1)
		for j := n - 1; j > 0; j-- {
			k := int(mix(x+uint64(j)) % uint64(j+1))
			perm[j], perm[k] = perm[k], perm[j]
		}
		pos := make([]int32, n)
		for p, cell := range perm {
			pos[cell] = int32(p)
		}
		m[s] = pos
	}
	return m
}()

// names are the rows' names, as many as the largest size has.
var names = func() []string {
	n := make([]string, Large.Rows())
	for i := range n {
		n[i] = fmt.Sprintf("node-%03d", i+1)
	}
	return n
}()

// epoch is how often the cell at place p in the order has changed by the
// step, when k cells of n change in each step.
func epoch(p, n, k, step int64) int64 {
	cursor := step * k
	if cursor <= p {
		return 0
	}
	return (cursor-p-1)/n + 1
}

// mix is SplitMix64's finaliser: a well-spread hash of x.
func mix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ x>>30) * 0xbf58476d1ce4e5b9
	x = (x ^ x>>27) * 0x94d049bb133111eb
	return x ^ x>>31
}

// unit is a number in [0, 1) from a hash.
func unit(h uint64) float64 { return float64(h>>11) / (1 << 53) }

// cell formats the value of a cell in an epoch: around a level of its own
// for each row and column, varying from one epoch to the next.
func cell(row, col int, ep int64) string {
	level := unit(mix(uint64(row)<<8 | uint64(col)))
	u := unit(mix(uint64(row)<<40 ^ uint64(col)<<32 ^ uint64(ep)*0x9e3779b97f4a7c15))
	switch col {
	case 0: // requests a second
		return thousands(int64((300 + level*4500) * (0.82 + 0.36*u)))
	case 1: // median latency
		return strconv.FormatFloat((0.6+level*3.6)*(0.85+0.3*u), 'f', 1, 64)
	case 2: // tail latency
		return strconv.Itoa(int((8 + level*110) * (0.7 + 0.6*u)))
	case 3: // CPU
		return strconv.Itoa(min(99, max(1, int(8+level*80+(u-0.5)*20))))
	case 4: // heap
		return strconv.Itoa(int((48 + level*390) * (0.9 + 0.2*u)))
	}
	if u < 0.8 {
		return "0"
	}
	return strconv.Itoa(1 + int((u-0.8)*45))
}

// thousands formats n with a comma between thousands.
func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	if n < 0 || len(s) <= 3 {
		return s
	}
	b := make([]byte, 0, len(s)+len(s)/3)
	for i := range len(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			b = append(b, ',')
		}
		b = append(b, s[i])
	}
	return string(b)
}

// Payload is the table at one step.
type Payload struct {
	Size    Size
	Changes int // cells that change in each step, 0 when still
	Rows    []Row
}

// Row is one row of the table.
type Row struct {
	Name  string
	Cells [cols]Cell
}

// Cell is a value, and whether it changed in this step.
type Cell struct {
	Text string
	New  bool
}

// payload builds the table of the size at the step, as it is when k cells
// change in each step. With marks, the cells that changed in the step are
// marked new; a still table marks none.
func payload(s Size, k int, step int64, marks bool) Payload {
	pos := order[s]
	n := int64(len(pos))
	p := Payload{Size: s, Rows: make([]Row, s.Rows())}
	if marks {
		p.Changes = k
	}
	for r := range p.Rows {
		row := &p.Rows[r]
		row.Name = names[r]
		for c := range cols {
			at := int64(pos[r*cols+c])
			ep := epoch(at, n, int64(k), step)
			row.Cells[c] = Cell{Text: cell(r, c, ep), New: marks && ep != epoch(at, n, int64(k), step-1)}
		}
	}
	return p
}

// payloadFor is the tab's table at the step. A still table shows the step
// it froze at, as the churn before had left it.
func payloadFor(st TabState, step int64) Payload {
	size, churn := st.size(), st.churn()
	if churn == Still {
		return payload(size, Changes(size, st.from()), st.Frozen, false)
	}
	return payload(size, Changes(size, churn), step, true)
}

// tableFor renders the tab's table at the step: the patch of its frame.
func tableFor(ctx context.Context, st TabState, step int64) ([]byte, error) {
	if st.churn() == Still {
		return demo.Patch(ctx, Table(payloadFor(st, step)))
	}
	return frames.get(ctx, key{st.size(), st.churn(), step})
}

// key names a frame of a moving table.
type key struct {
	size  Size
	churn Churn
	step  int64
}

// memo keeps the newest frames of each size and churn, so that the tabs
// which show the same payload share one render of each step. It holds two
// steps of each, since the tabs' views render at phases of their own.
type memo struct {
	mu sync.Mutex
	m  map[key][]byte
}

var frames = &memo{m: map[key][]byte{}}

func (c *memo) get(ctx context.Context, k key) ([]byte, error) {
	c.mu.Lock()
	b, ok := c.m[k]
	c.mu.Unlock()
	if ok {
		return b, nil
	}
	b, err := demo.Patch(ctx, Table(payload(k.size, Changes(k.size, k.churn), k.step, true)))
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for o := range c.m {
		if o.size == k.size && o.churn == k.churn && o.step < k.step-1 {
			delete(c.m, o)
		}
	}
	if len(c.m) < 2*len(Sizes)*len(Churns) {
		c.m[k] = b
	}
	return b, nil
}

// caption says what the table holds and how often it changes.
func caption(p Payload) string {
	cells := len(p.Rows) * cols
	s := strconv.Itoa(len(p.Rows)) + " rows, " + thousands(int64(cells)) + " cells. "
	switch p.Changes {
	case 0:
		return s + "Still: nothing changes, so your stream sends nothing. After 20 s of silence the hub sends a heartbeat, and after a minute it gives a zstd encoder back to the pool; pick a churn again, and the next report shows both."
	case 1:
		return s + "Every 250 ms one cell changes."
	}
	return s + "Every 250 ms " + thousands(int64(p.Changes)) + " cells change."
}
