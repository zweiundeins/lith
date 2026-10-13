// Command docgen writes the docs' generated parts: the package reference,
// from the Go doc comments of lith's packages, and the rows of the charts
// drawn from lith's benchmark CSV files. go:embed cannot reach outside the
// site's module, so the docs hold a copy, and internal/docs/gen's
// TestUpToDate fails when it drifts. Run it from anywhere inside lith's
// repository:
//
//	go generate ./content
package main

import (
	"fmt"
	"os"

	"github.com/zweiundeins/lith/site/internal/docs/gen"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "docgen:", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := gen.Root(".")
	if err != nil {
		return err
	}
	files, err := gen.Generate(root)
	if err != nil {
		return err
	}
	changed, err := gen.Write(root, files)
	for _, path := range changed {
		fmt.Println("docgen: wrote", path)
	}
	return err
}
