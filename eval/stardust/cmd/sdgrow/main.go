// Command sdgrow follows the size of a Stardust file while cards keep
// moving, with and without history retention of one transaction per card.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"stardust"
	"stardusteval/internal/kanban"
	"stardusteval/internal/sdx"
)

var (
	commits = flag.Int("commits", 10000, "commits, each moving ten cards")
	retain  = flag.Bool("retain", false, "set stardust/history {retain 1} on every card first")
	settle  = flag.Duration("settle", 35*time.Second, "wait at the end for the purge")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sdgrow:", err)
		os.Exit(1)
	}
}

func run() error {
	if err := sdx.Activate("license.token"); err != nil {
		return err
	}
	path := filepath.Join("/tmp/sdeval-db", fmt.Sprintf("grow-%d.db", os.Getpid()))
	defer os.Remove(path)
	db, err := stardust.Open(path, stardust.Options{Plugins: sdx.DefaultPlugins()})
	if err != nil {
		return err
	}
	defer db.Close()
	size := func() int64 {
		st, _ := os.Stat(path)
		return st.Size()
	}
	seed := map[string]any{}
	for i := range kanban.Cards {
		c := kanban.Seed(i)
		fields := map[string]any{"board": 1, "title": c.Title, "lane": c.Lane, "pos": c.Pos, "version": c.Version}
		if *retain {
			fields["stardust/history"] = map[string]any{"retain": 1}
		}
		seed["#_c"+strconv.Itoa(i)] = fields
	}
	commit, err := stardust.MergePatchValue(db, seed, nil)
	if err != nil {
		return err
	}
	ids := make([]int64, kanban.Cards)
	for i := range ids {
		ids[i] = commit.IDs["#_c"+strconv.Itoa(i)]
	}
	fmt.Printf("retain=%v seed: %d bytes\n", *retain, size())
	start := time.Now()
	version := int64(0)
	for i := range *commits {
		patch := map[string]any{}
		for k := range 10 {
			version++
			patch[strconv.FormatInt(ids[(i*10+k)%kanban.Cards], 10)] = map[string]any{"lane": int64((i + k) % kanban.Lanes), "version": version}
		}
		if _, err := stardust.MergePatchValue(db, patch, nil); err != nil {
			return err
		}
		if (i+1)%(*commits/5) == 0 {
			fmt.Printf("  after %6d commits (%7d facts, %v): %9d bytes\n", i+1, (i+1)*20, time.Since(start).Round(time.Second), size())
		}
	}
	time.Sleep(*settle)
	fmt.Printf("  %v later: %d bytes\n", *settle, size())
	return nil
}
