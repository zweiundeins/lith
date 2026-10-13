// Command txcheck looks at how a query reaches a fact's transaction and its
// commit time.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"stardust"
	"stardusteval/internal/sdx"
)

func main() {
	if err := sdx.Activate("license.token"); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	path := filepath.Join("/tmp/sdeval-db", fmt.Sprintf("txcheck-%d.db", os.Getpid()))
	defer os.Remove(path)
	db, err := stardust.Open(path, stardust.Options{})
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer db.Close()
	if _, err := stardust.MergePatchValue(db, map[string]any{
		"#_a": map[string]any{"lane": 3},
		"#_tx": map[string]any{"author": "probe"},
	}, nil); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	for _, text := range []string{
		"find  [?c ?tx]\nwhere [[?c lane 3 ?tx]]",
		"find  [?tx ?at]\nwhere [[?c lane 3 ?tx]\n       [?tx stardust/committedAt ?at]]",
		"find  [?tx ?who]\nwhere [[?c lane 3 ?tx]\n       [?tx author ?who]]",
		"find  [?tx ?f ?v]\nwhere [[?c lane 3 ?tx]\n       [?tx ?f ?v]]",
		"find  [?tx ?x]\nwhere [[?c lane 3 ?tx]\n       [?tx stardust/transaction ?x]]",
		"find  [?tx ?x]\nwhere [[?c lane 3 ?tx]\n       [?tx committedAt ?x]]",
		"find  [?e ?at]\nwhere [[?e stardust/committedAt ?at]]",
	} {
		q, err := db.Query(sdx.Doc(text))
		if err != nil {
			fmt.Printf("%q: open %v\n", text, err)
			continue
		}
		rows, err := q.Run(nil, stardust.RunOptions{}, nil)
		if err != nil {
			fmt.Printf("%q: run %v\n", text, err)
			q.Close()
			continue
		}
		all, err := rows.AllPages()
		fmt.Printf("%q:\n  %s %v\n", text, strings.Join(strings.Fields(sdx.Text(all)), " "), err)
		all.Close()
		rows.Close()
		q.Close()
	}
}
