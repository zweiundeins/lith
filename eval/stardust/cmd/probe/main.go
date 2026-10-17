// Command probe checks the Stardust behaviour the benchmark relies on and
// prints what it finds.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"stardust"
	"stardusteval/internal/sdx"
)

var (
	token     = flag.String("token", "license.token", "licence token file")
	dir       = flag.String("dir", "/tmp/sdeval-db", "directory for database files")
	pluginSet = flag.String("plugins", "all", "plugins to enable: all (history and reactors), none, history, reactors")
	second    = flag.Bool("second", false, "try a second open of the same file")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
}

func run() error {
	fmt.Println("abi version:", stardust.Version())
	path := filepath.Join(*dir, "probe.db")
	_, err := stardust.Open(path, stardust.Options{})
	fmt.Println("open before activation:", err)
	_, err = stardust.Parse([]byte("{a 1}"), stardust.DUST)
	fmt.Println("parse before activation:", err)
	fmt.Println("activate empty:", stardust.Activate(""))
	fmt.Println("activate garbage:", stardust.Activate("v4.public.garbage"))
	fmt.Println(sdx.LicenseSummary())
	if err := sdx.Activate(*token); err != nil {
		return err
	}
	fmt.Println(sdx.LicenseSummary())

	plugins, err := stardust.Plugins()
	if err != nil {
		return err
	}
	fmt.Println("plugins:", sdx.Text(plugins))
	plugins.Close()

	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	_ = os.Remove(path)
	opts := stardust.Options{Plugins: sdx.DefaultPlugins()}
	switch *pluginSet {
	case "none":
		opts.Plugins = map[string]any{}
	case "history":
		opts.Plugins = map[string]any{"history": map[string]any{}}
	case "reactors":
		opts.Plugins = map[string]any{"reactors": map[string]any{}}
	}
	db, err := stardust.Open(path, opts)
	if err != nil {
		return err
	}
	defer db.Close()

	if *second {
		_, err = stardust.Open(path, stardust.Options{Plugins: map[string]any{}})
		fmt.Println("second open in the same process:", err)
	}

	seed := sdx.Doc(`#_a {board 1
     title Alpha
     lane  0
     version 0}
#_b {board 1 title Beta lane 1 version 0}
#_tx {author probe}`)
	c, err := db.MergePatch(seed, nil)
	seed.Close()
	if err != nil {
		return err
	}
	fmt.Printf("seed commit: %+v\n", c)
	a, b := c.IDs["#_a"], c.IDs["#_b"]

	board := sdx.Doc(`find    [?e ?title ?lane ?version]
where   [[?e board 1]
         [?e title ?title]
         [?e lane ?lane]
         [?e version ?version]]
orderBy [?title]`)
	q, err := db.Query(board)
	board.Close()
	if err != nil {
		return err
	}
	defer q.Close()
	show := func(label string) error {
		rows, err := q.Run(nil, stardust.RunOptions{}, nil)
		if err != nil {
			return err
		}
		defer rows.Close()
		all, err := rows.AllPages()
		if err != nil {
			return err
		}
		defer all.Close()
		fmt.Println(label, sdx.Text(all))
		return nil
	}
	if err := show("board:"); err != nil {
		return err
	}

	try := func(label, text string, params any) {
		def, err := stardust.Parse([]byte(text), stardust.DUST)
		if err != nil {
			fmt.Println(label, "parse:", err)
			return
		}
		defer def.Close()
		m, err := db.Mutation(def)
		if err != nil {
			fmt.Println(label, "open:", err)
			return
		}
		defer m.Close()
		var ex stardust.MutationExplain
		c, err := stardust.RunMutationValue(m, params, stardust.RunOptions{}, &ex)
		fmt.Printf("%s: commit %+v err %v attempts %d\n", label, c, err, ex.Attempts)
	}
	cas1 := `query {find  [?card]
       where [[?card version ?seen]
              [?card lane ?old]]}
patch {?card {lane    ?lane
              version ?next}}`
	try("cas, lane and next as plain parameters", cas1,
		map[string]any{"?card": stardust.Link(a), "?seen": 0, "?lane": 2, "?next": 1})
	cas2 := `query {find  [?card ?to ?next]
       where [[?card version ?seen]
              [?card lane ?old]
              [[+ ?seen 1] ?next]
              [[+ ?lane 0] ?to]]}
patch {?card {lane    ?to
              version ?next}}`
	try("cas, derived next", cas2,
		map[string]any{"?card": stardust.Link(b), "?seen": 0, "?lane": 3})
	try("cas, stale version", cas2,
		map[string]any{"?card": stardust.Link(b), "?seen": 0, "?lane": 2})
	if err := show("board after cas:"); err != nil {
		return err
	}

	multi := sdx.Doc(fmt.Sprintf(`%d {lane 1 version 2}
%d {lane 0 version 2}`, a, b))
	c, err = db.MergePatch(multi, nil)
	multi.Close()
	fmt.Printf("two entities in one patch: %+v err %v\n", c, err)
	dup := sdx.Doc(fmt.Sprintf(`%d {lane 3}
%d {version 3}`, a, a))
	c, err = db.MergePatch(dup, nil)
	dup.Close()
	fmt.Printf("same entity twice in one patch: %+v err %v\n", c, err)
	same := sdx.Doc(fmt.Sprintf(`%d {lane 1 version 2}`, b))
	_ = same
	if err := show("board after patches:"); err != nil {
		return err
	}

	asOf := sdx.Doc(`find  [?title ?lane ?version]
where [[?e title ?title]
       [?e lane ?lane]
       [?e version ?version]]
orderBy [?title]
bind  {with {db {asOf ?at}}}`)
	qa, err := db.Query(asOf)
	asOf.Close()
	if err != nil {
		fmt.Println("asOf query:", err)
	} else {
		defer qa.Close()
		for _, at := range []int64{1, 2, 3, 4} {
			rows, err := stardust.RunQueryValue(qa, map[string]any{"?at": at}, stardust.RunOptions{}, nil)
			if err != nil {
				fmt.Println("asOf", at, err)
				continue
			}
			all, err := rows.AllPages()
			rows.Close()
			if err != nil {
				fmt.Println("asOf", at, err)
				continue
			}
			fmt.Println("asOf", at, sdx.Text(all))
			all.Close()
		}
	}

	sub, err := q.Subscribe(nil, stardust.RunOptions{})
	if err != nil {
		return err
	}
	defer sub.Close()
	ev, err := sub.Next(context.Background(), time.Second)
	if err != nil || ev == nil {
		return fmt.Errorf("first event: %v %v", ev, err)
	}
	fmt.Printf("event %d superseded %d tx %d\n", ev.Sequence, ev.Superseded, ev.Transaction)
	ev.Result.Close()
	ev, err = sub.Next(context.Background(), 0)
	fmt.Println("poll without change:", ev, err)

	unrelated := sdx.Doc(`#_room {floor 3}`)
	c, err = db.MergePatch(unrelated, nil)
	unrelated.Close()
	fmt.Printf("unrelated commit tx %d err %v\n", c.Transaction, err)
	ev, err = sub.Next(context.Background(), 200*time.Millisecond)
	if ev != nil {
		fmt.Printf("after unrelated commit: event %d tx %d\n", ev.Sequence, ev.Transaction)
		ev.Result.Close()
	} else {
		fmt.Println("after unrelated commit: no event", err)
	}
	title := sdx.Doc(fmt.Sprintf(`%d {title Alpha2}`, a))
	start := time.Now()
	c, err = db.MergePatch(title, nil)
	title.Close()
	committed := time.Since(start)
	ev, err2 := sub.Next(context.Background(), time.Second)
	if ev != nil {
		fmt.Printf("after title commit tx %d (commit %v, event after %v): event %d tx %d superseded %d\n", c.Transaction, committed, time.Since(start), ev.Sequence, ev.Transaction, ev.Superseded)
		ev.Result.Close()
	} else {
		fmt.Println("after title commit: no event", err, err2)
	}
	for i := range 3 {
		p := sdx.Doc(fmt.Sprintf(`%d {version %d}`, a, 10+i))
		if _, err := db.MergePatch(p, nil); err != nil {
			return err
		}
		p.Close()
	}
	ev, err = sub.Next(context.Background(), time.Second)
	if ev != nil {
		fmt.Printf("after three quick commits: event %d tx %d superseded %d\n", ev.Sequence, ev.Transaction, ev.Superseded)
		ev.Result.Close()
	}
	ev, err = sub.Next(context.Background(), 100*time.Millisecond)
	if ev != nil {
		fmt.Printf("then: event %d tx %d superseded %d\n", ev.Sequence, ev.Transaction, ev.Superseded)
		ev.Result.Close()
	} else {
		fmt.Println("then: no further event", err)
	}

	var se *stardust.Error
	_, err = db.MergePatch(sdx.Doc(`999999 {lane 1}`), nil)
	if errors.As(err, &se) {
		fmt.Printf("unknown entity: status %v code %q\n", se.Status, se.Code)
	} else {
		fmt.Println("unknown entity:", err)
	}
	return nil
}
