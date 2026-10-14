package app_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/site/internal/browser"
	"github.com/zweiundeins/lith/sse"
)

// reopenPage opens its stream as the site's pages do, from an element with
// an id, with retry 'error', and shows the view's number.
const reopenPage = `<!doctype html><title>reopen</title><link rel="icon" href="data:,">
<script type="module" src="/datastar.js"></script>
<div id="stream" hidden data-init="@get('/stream', {retry: 'error'})"></div>
<div id="view"></div>`

// TestReopenForABetterTier runs, in Chromium with the site's Datastar, a
// hub whose budget has no room for the window a tab's 230 KB frames need:
// its brotli stream takes 2^16 at quality 3. Once room frees up, the hub
// ends the stream with sse.Reconnect while the server keeps running; the
// page opens it again, the new stream gets 2^18, and the page goes on
// showing the view's frames.
//
//	LITH_CHROME_NO_SANDBOX=1 go test ./internal/app -run TestReopenForABetterTier
func TestReopenForABetterTier(t *testing.T) {
	datastar, err := os.ReadFile(filepath.Join("..", "web", "static", "vendor", "datastar-rocket.js"))
	if err != nil {
		t.Fatal(err)
	}
	b := browser.Start(t)
	budget := sse.NewBudget(sse.BudgetOptions{Limit: 8 << 20})
	h := live.New(live.Options{Tick: 5 * time.Millisecond, SSE: sse.Options{Budget: budget}})
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	go func() { h.Run(ctx); close(ran) }()
	var version atomic.Int64
	render := func() []byte {
		var buf bytes.Buffer
		fmt.Fprintf(&buf, `<div id="view"><p id="n">%d</p>`, version.Load())
		for i := range 1300 {
			fmt.Fprintf(&buf, `<p class="row" data-key="%d" hidden>Row %d of the tab's table: a name, a class, a constellation, and a value of %d, in light years</p>`, i, i, i*7)
		}
		buf.WriteString(`</div>`)
		return sse.PatchElements(buf.Bytes())
	}
	view := live.View{Key: "tab", Topics: []string{"tab"}, Render: func(context.Context, *sql.Tx) (any, error) { return render(), nil }}
	var opened atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(reopenPage))
	})
	mux.HandleFunc("GET /datastar.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		w.Write(datastar)
	})
	mux.HandleFunc("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		opened.Add(1)
		// Chromium takes zstd too, which moves while open; this is about
		// brotli, as Safari's and Firefox's tabs get it.
		r.Header.Set("Accept-Encoding", sse.Brotli)
		h.Serve(w, r, live.Spec{ID: "tab", Views: []live.View{view}, Reconnect: "stream"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		cancel()
		<-ran
	})

	// What is left after the tab's frame and its stream at 2^16 (752 KiB
	// with the stream's own 32): 200 KiB, too little for 2^18.
	fill := int64(8<<20) - 752<<10 - int64(len(render())+8<<10) - 200<<10
	budget.Hold(fill)
	p := b.NewPage(t, 800, 600)
	p.Navigate(t, srv.URL+"/")
	p.Wait(t, `document.getElementById('n')?.textContent === '0'`, 10*time.Second)
	if got := budget.Stats().Streams; got["br q3 2^16"] != 1 {
		t.Fatalf("the first stream: %v", got)
	}
	budget.Hold(-fill)
	waitFor(t, "the stream opened again", func() bool { return opened.Load() == 2 })
	waitFor(t, "the new stream at 2^18", func() bool { return budget.Stats().Streams["br q4 2^18"] == 1 })
	for i := 1; i <= 3; i++ {
		version.Store(int64(i))
		h.Changed([]string{"tab"})
		p.Wait(t, `document.getElementById('n')?.textContent === '`+strconv.Itoa(i)+`'`, 10*time.Second)
	}
	if st := budget.Stats(); st.Reopened != 1 || opened.Load() != 2 {
		t.Errorf("reopened %d, %d streams opened", st.Reopened, opened.Load())
	}
	if errs := p.Errors(); len(errs) > 0 {
		t.Errorf("%d errors:\n%v", len(errs), errs)
	}
}
