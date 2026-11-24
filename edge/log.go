package edge

import (
	"cmp"
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync/atomic"
	"time"
)

// Recover turns a panic in next into a 500, logged with its stack. When the
// response has already begun, it aborts the connection instead, so the client
// can tell the response is incomplete. It is RecoverWith without options.
func Recover(log *slog.Logger, next http.Handler) http.Handler {
	return RecoverWith(log, next, RecoverOptions{})
}

// RecoverOptions configures RecoverWith.
type RecoverOptions struct {
	// Page writes the app's error page for a navigation (Navigation) whose
	// handler panicked before it wrote anything. The status is 500 whatever
	// Page writes, and the response is not cached. A Page that writes
	// nothing, or panics before it writes, leaves the plain 500; one that
	// panics after it began aborts the connection.
	Page func(w http.ResponseWriter, r *http.Request)
}

// representation are the headers that describe the body a handler meant to
// send, which a 500 replaces: a Content-Disposition left on it would make the
// browser download the error page.
var representation = []string{"Content-Encoding", "Content-Disposition", "Content-Language", "Content-Location", "Content-Range", "ETag", "Last-Modified"}

// RecoverWith is Recover with options: Page renders the app's own 500 page
// for a navigation.
func RecoverWith(log *slog.Logger, next http.Handler, o RecoverOptions) http.Handler {
	log = cmp.Or(log, slog.Default())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &recorder{ResponseWriter: w}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			log.LogAttrs(r.Context(), slog.LevelError, "panic",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Any("panic", v),
				slog.String("stack", string(debug.Stack())))
			if rec.status != 0 {
				panic(http.ErrAbortHandler)
			}
			h := w.Header()
			for _, k := range representation {
				h.Del(k)
			}
			h.Set("Cache-Control", "no-store")
			if o.Page != nil && Navigation(r) && errorPage(log, o.Page, w, r) {
				return
			}
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}()
		next.ServeHTTP(rec, r)
	})
}

// errorPage runs page with its status fixed at 500 and reports whether it
// wrote the response.
func errorPage(log *slog.Logger, page func(http.ResponseWriter, *http.Request), w http.ResponseWriter, r *http.Request) (wrote bool) {
	pw := &status500{ResponseWriter: w}
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		log.LogAttrs(r.Context(), slog.LevelError, "panic in the error page",
			slog.String("path", r.URL.Path),
			slog.Any("panic", v),
			slog.String("stack", string(debug.Stack())))
		if pw.wrote {
			panic(http.ErrAbortHandler)
		}
		wrote = false
	}()
	page(pw, r)
	return pw.wrote
}

// status500 sends 500 whatever status its writer asks for.
type status500 struct {
	http.ResponseWriter
	wrote bool
}

func (w *status500) WriteHeader(int) {
	if !w.wrote {
		w.wrote = true
		w.ResponseWriter.WriteHeader(http.StatusInternalServerError)
	}
}

func (w *status500) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusInternalServerError)
	return w.ResponseWriter.Write(p)
}

func (w *status500) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Navigation reports whether r loads a page into a browser's window: a GET
// or HEAD whose Sec-Fetch-Dest is document, or that sends none (an older
// browser, a link opened from another program), and that is no Datastar
// request. A fetch from a page sends Sec-Fetch-Dest empty, an image or a
// script its own destination.
func Navigation(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if r.Header.Get("Datastar-Request") != "" {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Dest") {
	case "", "document":
		return true
	}
	return false
}

// LogRequests logs a line for each request as it ends: method, path, status,
// bytes, duration and client. A 5xx, or a response a panic cut off, logs at
// Error, the rest at Info, or at Debug when the handler called Quiet. Mount
// it outside Recover, so it logs the 500.
func LogRequests(log *slog.Logger, next http.Handler) http.Handler {
	log = cmp.Or(log, slog.Default())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w}
		completed := false
		quiet := new(atomic.Bool)
		defer func() {
			status, level := rec.status, slog.LevelInfo
			if status == 0 && completed {
				status = http.StatusOK
			}
			if status >= 500 || !completed {
				level = slog.LevelError
			} else if quiet.Load() {
				level = slog.LevelDebug
			}
			ip := "local"
			if a := ClientIP(r); a.IsValid() {
				ip = a.String()
			}
			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int64("bytes", rec.bytes),
				slog.Duration("took", time.Since(start)),
				slog.String("ip", ip),
			}
			if !completed {
				attrs = append(attrs, slog.Bool("aborted", true))
			}
			log.LogAttrs(r.Context(), level, "request", attrs...)
		}()
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), quietKey{}, quiet)))
		completed = true
	})
}

type quietKey struct{}

// Quiet tells LogRequests that the request is one of many alike, such as
// the moves of a pointer that a page sends several times a second: its
// line goes out at Debug instead of Info, unless it fails with a 5xx or is
// cut off, which still log at Error. Outside LogRequests it does nothing.
func Quiet(r *http.Request) {
	if q, ok := r.Context().Value(quietKey{}).(*atomic.Bool); ok {
		q.Store(true)
	}
}

// recorder notes the status and size of a response as it passes.
type recorder struct {
	http.ResponseWriter
	status int // the final one, 0 before
	bytes  int64
}

func (rw *recorder) WriteHeader(code int) {
	if rw.status == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
		rw.status = code
	}
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *recorder) Write(p []byte) (int, error) {
	if rw.status == 0 {
		rw.status = http.StatusOK
	}
	n, err := rw.ResponseWriter.Write(p)
	rw.bytes += int64(n)
	return n, err
}

func (rw *recorder) Flush() { rw.FlushError() }

func (rw *recorder) FlushError() error {
	if rw.status == 0 {
		rw.status = http.StatusOK
	}
	return http.NewResponseController(rw.ResponseWriter).Flush()
}

func (rw *recorder) Unwrap() http.ResponseWriter { return rw.ResponseWriter }
