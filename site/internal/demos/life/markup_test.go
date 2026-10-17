package life

import (
	"bytes"
	"context"
	"math/rand/v2"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Every markup draws the same board: the same cells in the same letters.
func TestMarkups(t *testing.T) {
	b := Board{Cells: make([]byte, Cells), History: "[]"}
	Soup(b.Cells, rand.New(rand.NewPCG(1, 2)))
	b.step(1000)
	b.step(1100)
	want := string(codes(b))
	ctx := context.Background()
	for _, name := range Markups {
		for _, noStats := range []bool{false, true} {
			m := Markup{Board: name, NoStats: noStats}
			f, err := m.frame(ctx, b, 1, time.UnixMilli(2000))
			if err != nil {
				t.Fatal(err)
			}
			s := string(f)
			if name == "canvas" {
				if !strings.Contains(s, `"c":"`+want+`"`) {
					t.Errorf("%s: the signal does not hold the cells", name)
				}
			} else if !strings.Contains(s, `id="life-board"`) {
				t.Errorf("%+v: no board", m)
			}
			if strings.Contains(s, `id="life-stats"`) == noStats {
				t.Errorf("%+v: counters %v", m, !noStats)
			}
		}
	}
	if _, err := (Markup{Board: "nope"}).Demo(); err == nil {
		t.Error("an unknown markup made a demo")
	}
}

var (
	startTag  = regexp.MustCompile(`<[a-zA-Z][^>]*>`)
	attribute = regexp.MustCompile(`\s[a-zA-Z_:][-\w:.]*(=("[^"]*"|'[^']*'|[^\s"'>]+))?`)
)

// TestFrameAnatomy measures each markup's frames over 600 generations of
// a soup, with a new soup every 150 as in TestLifeDrift: the bytes of the
// events, the elements and attributes in them, and the bytes per frame
// after zstd at its fastest level with a 2^19 window, one stream for all
// frames, flushed after each, as lith's streams send them. Set
// FRAME_ANATOMY=1 to run it.
func TestFrameAnatomy(t *testing.T) {
	if os.Getenv("FRAME_ANATOMY") == "" {
		t.Skip("set FRAME_ANATOMY=1")
	}
	ctx := context.Background()
	t.Logf("| markup | counters | event bytes | elements | attributes | zstd bytes |")
	for _, name := range Markups {
		for _, noStats := range []bool{false, true} {
			m := Markup{Board: name, NoStats: noStats}
			rng := rand.New(rand.NewPCG(7, 7))
			b := Board{Cells: make([]byte, Cells), History: "[]"}
			var out bytes.Buffer
			z, _ := zstd.NewWriter(&out, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithWindowSize(1<<19), zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(false))
			var raw, els, attrs, packed int
			const n = 600
			now := int64(1_000_000)
			for g := range n {
				if g%150 == 0 {
					Soup(b.Cells, rng)
					b.Trail = nil
				}
				now += 100
				b.step(now)
				f, err := m.frame(ctx, b, 1, time.UnixMilli(now))
				if err != nil {
					t.Fatal(err)
				}
				raw += len(f)
				for _, tag := range startTag.FindAll(f, -1) {
					els++
					attrs += len(attribute.FindAll(tag, -1))
				}
				before := out.Len()
				z.Write(f)
				z.Flush()
				packed += out.Len() - before
			}
			t.Logf("| %s | %v | %d | %d | %d | %d |", name, !noStats, raw/n, els/n, attrs/n, packed/n)
		}
	}
}
