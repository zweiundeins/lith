package gen

import (
	"encoding/csv"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/zweiundeins/lith/site/internal/markdown"
)

// chartBlock finds the ```chart blocks of a page: the fence's line, the
// body and the closing fence.
var chartBlock = regexp.MustCompile("(?s)(```chart\n)(.*?)(```)")

// Charts returns each page with a chart drawn from a CSV file, its rows
// filled from that file. Such a chart's spec names its columns:
//
//	source: bench/budget/simulation.csv
//	columns: policy, open, encoder_heap_mib   (series, x and y)
//	where: workload=board, window!=0           (rows to keep, by column)
//	series: No budget=lith default; Budget of 128 MiB=budget
//	step: 50                                   (keep the x that are multiples of it)
//
// series names the series to keep, in order, each as label=value, the
// label being what the chart shows.
func Charts(root string) (map[string][]byte, error) {
	files := map[string][]byte{}
	docs := filepath.Join(root, filepath.FromSlash(Docs))
	err := filepath.WalkDir(docs, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		generated := false
		var errs []error
		out := chartBlock.ReplaceAllStringFunc(string(src), func(block string) string {
			m := chartBlock.FindStringSubmatch(block)
			spec := markdown.Spec(m[2])
			meta := metaOf(spec)
			if meta["columns"] == "" {
				return block
			}
			rows, err := fill(root, meta["source"], meta)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", path, err))
				return block
			}
			generated = true
			return m[1] + spec + "\n---\n" + rows + m[3]
		})
		if len(errs) > 0 {
			return errs[0]
		}
		if generated {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			files[filepath.ToSlash(rel)] = []byte(out)
		}
		return nil
	})
	return files, err
}

// metaOf reads the keys of a chart's spec.
func metaOf(spec string) map[string]string {
	m := map[string]string{}
	for line := range strings.Lines(spec) {
		if k, v, ok := strings.Cut(line, ":"); ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

// fill returns a chart's rows from its CSV file, as CSV.
func fill(root, source string, meta map[string]string) (string, error) {
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(source)))
	if err != nil {
		return "", err
	}
	defer f.Close()
	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return "", fmt.Errorf("%s: %w", source, err)
	}
	if len(records) < 2 {
		return "", fmt.Errorf("%s: no rows", source)
	}
	col := map[string]int{}
	for i, name := range records[0] {
		col[name] = i
	}
	pick := func(name string) (int, error) {
		i, ok := col[strings.TrimSpace(name)]
		if !ok {
			return 0, fmt.Errorf("%s has no column %q", source, name)
		}
		return i, nil
	}
	cols := strings.Split(meta["columns"], ",")
	if len(cols) != 3 {
		return "", fmt.Errorf("columns %q: want series, x and y", meta["columns"])
	}
	var idx [3]int
	for i, c := range cols {
		if idx[i], err = pick(c); err != nil {
			return "", err
		}
	}
	type filter struct {
		col   int
		value string
		not   bool
	}
	var where []filter
	if w := meta["where"]; w != "" {
		for _, cond := range strings.Split(w, ",") {
			k, v, ok := strings.Cut(cond, "=")
			if !ok {
				return "", fmt.Errorf("where %q: want column=value or column!=value", cond)
			}
			k, not := strings.CutSuffix(k, "!")
			i, err := pick(k)
			if err != nil {
				return "", err
			}
			where = append(where, filter{i, strings.TrimSpace(v), not})
		}
	}
	type series struct{ label, value string }
	var keep []series
	for _, s := range strings.Split(meta["series"], ";") {
		label, value, ok := strings.Cut(s, "=")
		if !ok {
			return "", fmt.Errorf("series %q: want label=value", s)
		}
		keep = append(keep, series{strings.TrimSpace(label), strings.TrimSpace(value)})
	}
	step := 0.0
	if s := meta["step"]; s != "" {
		if step, err = strconv.ParseFloat(s, 64); err != nil || step <= 0 {
			return "", fmt.Errorf("step %q is not a positive number", s)
		}
	}
	var b strings.Builder
	w := csv.NewWriter(&b)
	for _, s := range keep {
		n := 0
		for _, rec := range records[1:] {
			if rec[idx[0]] != s.value || slices.ContainsFunc(where, func(f filter) bool { return (rec[f.col] == f.value) == f.not }) {
				continue
			}
			if step > 0 {
				x, err := strconv.ParseFloat(rec[idx[1]], 64)
				if err != nil || x/step != float64(int64(x/step)) {
					continue
				}
			}
			if err := w.Write([]string{s.label, rec[idx[1]], rec[idx[2]]}); err != nil {
				return "", err
			}
			n++
		}
		if n == 0 {
			return "", fmt.Errorf("%s: no rows for the series %q", source, s.value)
		}
	}
	w.Flush()
	return b.String(), w.Error()
}
