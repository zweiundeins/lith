// Command subcheck looks at which commits wake a subscription: per field, per
// entity, or per value of a literal clause.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"stardust"
	"stardusteval/internal/sdx"
)

func main() {
	if err := sdx.Activate("license.token"); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	path := filepath.Join("/tmp/sdeval-db", fmt.Sprintf("subcheck-%d.db", os.Getpid()))
	defer os.Remove(path)
	db, err := stardust.Open(path, stardust.Options{})
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer db.Close()
	c, err := stardust.MergePatchValue(db, map[string]any{
		"#_a": map[string]any{"board": 1, "lane": 0, "version": 0},
		"#_b": map[string]any{"board": 2, "lane": 1, "version": 0},
	}, nil)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	a, b := c.IDs["#_a"], c.IDs["#_b"]
	next := func(label string, s *stardust.Subscription) {
		ev, err := s.Next(context.Background(), 300*time.Millisecond)
		if ev == nil {
			fmt.Printf("  %-34s no event (%v)\n", label, err)
			return
		}
		fmt.Printf("  %-34s event %d at tx %d: %s\n", label, ev.Sequence, ev.Transaction, oneLine(sdx.Text(ev.Result)))
		ev.Result.Close()
	}
	v := int64(100)
	patch := func(id int64, field string) {
		v++
		if _, err := stardust.MergePatchValue(db, map[string]any{fmt.Sprint(id): map[string]any{field: v}}, nil); err != nil {
			fmt.Println(err)
		}
	}
	cases := []struct {
		name, text string
		params     map[string]any
	}{
		{"board 1 by literal", "find  [?e ?version]\nwhere [[?e board 1]\n       [?e version ?version]]", nil},
		{"card a by parameter", "find  [?version]\nwhere [[?card version ?version]]", map[string]any{"?card": stardust.Link(a)}},
	}
	for _, k := range cases {
		fmt.Println(k.name + ":")
		q, err := db.Query(sdx.Doc(k.text))
		if err != nil {
			fmt.Println("  open:", err)
			continue
		}
		var p *stardust.Document
		if k.params != nil {
			p, _ = stardust.FromValue(k.params)
		}
		s, err := q.Subscribe(p, stardust.RunOptions{})
		p.Close()
		if err != nil {
			fmt.Println("  subscribe:", err)
			continue
		}
		next("first event", s)
		patch(b, "version")
		next("after b's version changed", s)
		patch(b, "lane")
		next("after b's lane changed", s)
		patch(a, "version")
		next("after a's version changed", s)
		s.Close()
		q.Close()
	}
}

func oneLine(s string) string {
	out := make([]rune, 0, len(s))
	space := false
	for _, r := range s {
		if r == '\n' || r == ' ' {
			if !space {
				out = append(out, ' ')
			}
			space = true
			continue
		}
		space = false
		out = append(out, r)
	}
	return string(out)
}
