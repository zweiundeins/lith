package canvas

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/demo"
	"github.com/zweiundeins/lith/site/internal/demo/demotest"
)

// BenchmarkPaint measures the pixels a second the writer takes: 100
// painters send Paint commands at once and wait for each commit, as the
// handlers do. Two cells is a stroke's batch at the pixel limit (20 a
// second, a batch every 80 ms), 60 the most one batch holds.
//
//	go test ./internal/demos/canvas -run - -bench Paint -benchtime 5s
func BenchmarkPaint(b *testing.B) {
	for _, cells := range []int{2, 60} {
		b.Run(fmt.Sprintf("cells=%d", cells), func(b *testing.B) {
			e := demotest.Open(b, Demo)
			var painter atomic.Int64
			procs := runtime.GOMAXPROCS(0)
			b.SetParallelism((100 + procs - 1) / procs) // GOMAXPROCS times this: 100 painters or a few more
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				id := painter.Add(1)
				v := demo.Viewer{Session: fmt.Sprint("s", id), Tab: fmt.Sprint("tab", id)}
				batch := make([]int, cells)
				n := int(id) * 37
				for pb.Next() {
					for i := range batch {
						batch[i] = (n + i) % Cells
					}
					n += cells
					if err := e.W.Do(context.Background(), Paint{Viewer: v, Color: 1 + int(id)%15, Cells: batch, At: time.Now()}); err != nil {
						b.Error(err)
						return
					}
				}
			})
			b.ReportMetric(float64(b.N*cells)/b.Elapsed().Seconds(), "pixels/s")
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "paints/s")
			st := e.W.Stats()
			b.ReportMetric(float64(st.Commands)/float64(max(1, st.Batches)), "paints/batch")
		})
	}
}
