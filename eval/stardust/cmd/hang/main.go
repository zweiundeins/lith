// Command hang narrows down where a first commit blocks.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"stardust"
	"stardusteval/internal/sdx"
)

var (
	token    = flag.String("token", "license.token", "licence token file")
	dir      = flag.String("dir", "/tmp/sdeval-db", "directory for database files")
	step     = flag.String("step", "patch", "what to try after open: query, patch")
	batch    = flag.Duration("commit-batch", 0, "Options.CommitBatch")
	fixStack = flag.Bool("fixstack", false, "lower the soft stack limit to 8 MiB before Open")
)

func threads() string {
	b, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "Threads:") {
			return strings.TrimSpace(strings.TrimPrefix(l, "Threads:"))
		}
	}
	return "?"
}

func main() {
	flag.Parse()
	if err := sdx.Activate(*token); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	if *fixStack {
		var lim syscall.Rlimit
		_ = syscall.Getrlimit(syscall.RLIMIT_STACK, &lim)
		lim.Cur = 8 << 20
		fmt.Println("setrlimit:", syscall.Setrlimit(syscall.RLIMIT_STACK, &lim))
	}
	path := filepath.Join(*dir, "hang.db")
	_ = os.Remove(path)
	fmt.Println("threads before open:", threads())
	db, err := stardust.Open(path, stardust.Options{CommitBatch: *batch})
	if err != nil {
		fmt.Println("open:", err)
		os.Exit(1)
	}
	fmt.Println("threads after open:", threads())
	time.Sleep(200 * time.Millisecond)
	fmt.Println("threads 200ms after open:", threads())
	switch *step {
	case "query":
		q, err := db.Query(sdx.Doc("find  [?e]\nwhere [[?e x ?v]]"))
		if err != nil {
			fmt.Println("query open:", err)
			break
		}
		rows, err := q.Run(nil, stardust.RunOptions{}, nil)
		if err != nil {
			fmt.Println("query run:", err)
			break
		}
		all, err := rows.AllPages()
		fmt.Println("query result:", sdx.Text(all), err)
	case "patch":
		done := make(chan struct{})
		go func() {
			c, err := db.MergePatch(sdx.Doc(`#_a {x 1}`), nil)
			fmt.Printf("patch: %+v %v\n", c, err)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			fmt.Println("patch still running after 3s; threads:", threads())
			st, _ := os.Stat(path)
			fmt.Println("file size:", st.Size())
			os.Exit(3)
		}
	}
	db.Close()
	fmt.Println("closed; threads:", threads())
}
