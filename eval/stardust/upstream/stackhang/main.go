// Command stackhang reproduces a write that never returns when the process
// runs with an unlimited stack limit (ulimit -s unlimited).
//
//	ulimit -s unlimited; go run ./upstream/stackhang license.token /tmp/hang.db   # blocks in MergePatchValue
//	ulimit -s 8192;      go run ./upstream/stackhang license.token /tmp/hang.db   # returns at once
package main

import (
	"fmt"
	"os"
	"strings"

	"stardust"
)

func main() {
	token, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	if err := stardust.Activate(strings.TrimSpace(string(token))); err != nil {
		panic(err)
	}
	os.Remove(os.Args[2])
	db, err := stardust.Open(os.Args[2], stardust.Options{})
	if err != nil {
		panic(err)
	}
	defer db.Close()
	fmt.Println("open; committing")
	commit, err := stardust.MergePatchValue(db, map[string]any{"#_a": map[string]any{"x": 1}}, nil)
	fmt.Println("committed:", commit.Transaction, err)
}
