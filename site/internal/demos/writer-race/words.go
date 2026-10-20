package writerrace

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// fresh is how long a race that ended counts as arriving: its result and
// its row in the table materialise for a tab that opens meanwhile too.
const fresh = 8 * time.Second

// state names the track's state for CSS: none, running, done, failed,
// interrupted.
func (t track) state() string {
	if t.Race.ID == 0 {
		return "none"
	}
	return t.Race.State
}

// status is the line beside the race's name.
func (t track) status() string {
	at := strconv.FormatFloat(t.Race.Last.At.Seconds(), 'f', 1, 64) + " s"
	switch t.Race.State {
	case "running":
		return "On the track, " + at
	case "done":
		return "Finished in " + at
	case "failed":
		return "Failed after " + at
	}
	return "Stopped after " + at + " by a restart of the server"
}

// rateNote says what the big number covers.
func (t track) rateNote() string {
	if t.Running {
		return "over the last quarter second, from writer.Stats every 50 ms"
	}
	return "over the whole race: commands divided by its time"
}

// perTx writes commands per transaction, rounded: "1 command", "122
// commands".
func perTx(f float64) string {
	if math.Round(f) == 1 {
		return "1 command"
	}
	return strconv.FormatFloat(f, 'f', 0, 64) + " commands"
}

func producers(n int) string {
	if n == 1 {
		return "1 producer"
	}
	return strconv.Itoa(n) + " producers"
}

// said is what the tab hears about its race. A start that found the
// session's race still in line or on the track says so while it is.
func said(o outcome) string {
	r := o.Race
	name := "Race " + strconv.FormatInt(r.ID, 10) + " is yours: "
	again := ""
	if o.State.Again {
		again = " One race at a time for each visitor."
	}
	switch r.State {
	case "queued":
		if o.Place <= 1 {
			return name + "next on the track." + again
		}
		return name + ordinal(o.Place) + " in line." + again
	case "running":
		return name + "on the track now." + again
	case "done":
		return name + group(int64(r.Rate())) + " commands a second, " + strconv.FormatFloat(r.PerTx(), 'f', 0, 64) + " to a transaction."
	case "failed":
		return name + "its writer failed."
	case "dropped":
		return name + "it waited too long and was dropped."
	}
	return name + "the server restarted during it."
}

func ordinal(n int) string {
	s := strconv.Itoa(n)
	switch {
	case n%100 >= 11 && n%100 <= 13:
		return s + "th"
	case n%10 == 1:
		return s + "st"
	case n%10 == 2:
		return s + "nd"
	case n%10 == 3:
		return s + "rd"
	}
	return s + "th"
}

// stopped is the table's word for a race without a rate.
func stopped(r Race) string {
	if r.State == "failed" {
		return "failed"
	}
	return "restart"
}

func (b bar) title() string {
	var size string
	switch b.Label {
	case "1":
		size = "1 command"
	case "128":
		size = "128 commands or more"
	default:
		n, _ := strconv.Atoi(b.Label)
		size = fmt.Sprintf("%d to %d commands", n, 2*n-1)
	}
	return size + ": " + group(b.N) + " transactions"
}

// gridPath draws the chart's three guide lines, at a quarter, half and
// three quarters of its height.
func gridPath() string {
	var b strings.Builder
	for _, f := range []int{1, 2, 3} {
		fmt.Fprintf(&b, "M0 %d.5H%d", chartH*f/4, chartW)
	}
	return b.String()
}

// caption is the chart's text for screen readers.
func (c chart) caption() string {
	if c.Bench == "" {
		return "."
	}
	return ", against " + c.Bench + " a second in lith's benchmark on this server."
}

// waiting counts the races in list that wait.
func waiting(list []Race) int {
	n := 0
	for _, r := range list {
		if r.State == "queued" {
			n++
		}
	}
	return n
}

func waitID(id int64) string   { return "writer-race-wait-" + strconv.FormatInt(id, 10) }
func resultID(id int64) string { return "writer-race-result-" + strconv.FormatInt(id, 10) }
