package edge

import (
	"cmp"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// Recover turns a panic in next into a 500, logged with its stack. When the
// response has already begun, it aborts the connection instead, so the client
// can tell the response is incomplete.
func Recover(log *slog.Logger, next http.Handler) http.Handler {
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
			for _, k := range []string{"Content-Encoding", "ETag", "Last-Modified"} {
				h.Del(k)
			}
			h.Set("Cache-Control", "no-store")
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}()
		next.ServeHTTP(rec, r)
	})
}

// LogRequests logs a line for each request as it ends: method, path, status,
// bytes, duration and client. A 5xx, or a response a panic cut off, logs at
// Error, the rest at Info. Mount it outside Recover, so it logs the 500.
func LogRequests(log *slog.Logger, next http.Handler) http.Handler {
	log = cmp.Or(log, slog.Default())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w}
		completed := false
		defer func() {
			status, level := rec.status, slog.LevelInfo
			if status == 0 && completed {
				status = http.StatusOK
			}
			if status >= 500 || !completed {
				level = slog.LevelError
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
		next.ServeHTTP(rec, r)
		completed = true
	})
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
