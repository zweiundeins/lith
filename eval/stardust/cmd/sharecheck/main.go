// Command sharecheck measures whether identical subscriptions share their
// reruns: n subscriptions to one board query, polled one after another by a
// single goroutine after each commit, against blocked consumers.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"stardust"
	"stardusteval/internal/kanban"
	"stardusteval/internal/sdx"
)

var n = flag.Int("n", 200, "subscriptions")

const boardText = `find    [?e ?title ?lane ?pos ?version]
where   [[?e board 1]
         [?e title ?title]
         [?e lane ?lane]
         [?e pos ?pos]
         [?e version ?version]]
orderBy [?lane ?pos]`

func cpu() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func main() {
	flag.Parse()
	if err := sdx.Activate("license.token"); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	path := filepath.Join("/tmp/sdeval-db", fmt.Sprintf("share-%d.db", os.Getpid()))
	defer os.Remove(path)
	db, err := stardust.Open(path, stardust.Options{})
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer db.Close()
	patch := map[string]any{}
	for i := range kanban.Cards {
		c := kanban.Seed(i)
		patch["#_c"+strconv.Itoa(i)] = map[string]any{"board": 1, "title": c.Title, "lane": c.Lane, "pos": c.Pos, "version": c.Version}
	}
	commit, err := stardust.MergePatchValue(db, patch, nil)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	card := strconv.FormatInt(commit.IDs["#_c0"], 10)
	q, err := db.Query(sdx.Doc(boardText))
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer q.Close()
	subs := make([]*stardust.Subscription, *n)
	for i := range subs {
		if subs[i], err = q.Subscribe(nil, stardust.RunOptions{}); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer subs[i].Close()
	}
	poll := func() (events int, superseded int64) {
		for _, s := range subs {
			ev, err := s.Next(context.Background(), 0)
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			if ev != nil {
				events++
				superseded += ev.Superseded
				ev.Result.Close()
			}
		}
		return
	}
	start := time.Now()
	c0 := cpu()
	events, _ := poll()
	fmt.Printf("first events: %d in %v, cpu %v\n", events, time.Since(start), cpu()-c0)
	for round := range 5 {
		if _, err := stardust.MergePatchValue(db, map[string]any{card: map[string]any{"version": round + 1}}, nil); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		time.Sleep(50 * time.Millisecond)
		start, c0 := time.Now(), cpu()
		events, superseded := poll()
		fmt.Printf("after commit %d: %d events, superseded %d, polled in %v, cpu %v\n", round+1, events, superseded, time.Since(start), cpu()-c0)
	}
	for round := range 3 {
		for k := range 3 {
			if _, err := stardust.MergePatchValue(db, map[string]any{card: map[string]any{"version": 100*(round+1) + k}}, nil); err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
		}
		time.Sleep(50 * time.Millisecond)
		start, c0 := time.Now(), cpu()
		events, superseded := poll()
		fmt.Printf("after three commits: %d events, superseded %d, polled in %v, cpu %v\n", events, superseded, time.Since(start), cpu()-c0)
	}
}
