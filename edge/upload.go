package edge

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Upload defaults.
const (
	// UploadWindow is the time an upload's body gets at first, and again
	// each time it has sent MinRate for that long.
	UploadWindow = 10 * time.Second

	// UploadMinRate is a minimum rate for an upload over a slow link, in
	// bytes a second: three uploads sharing a 0.4 Mbit/s mobile link get
	// about 133 kbit/s each, some 16 KB/s, twice its 8 KiB/s, while a
	// client that trickles a byte a second is cut off after a window.
	UploadMinRate = 8 << 10

	// UploadMax is the default size limit of an upload, 10 MiB.
	UploadMax = 10 << 20

	// UploadSlack is added to Max/MinRate for an upload's longest time, so
	// that an upload of the largest size at the lowest rate, with the
	// stalls of a mobile link, still finishes.
	UploadSlack = time.Minute
)

// UploadLimits configure Upload. A zero field takes its default.
type UploadLimits struct {
	Max     int64         // bytes; UploadMax
	MinRate int64         // bytes a second; UploadMinRate
	Window  time.Duration // UploadWindow
	Slack   time.Duration // UploadSlack
}

// Upload wraps r.Body for a handler that takes a large body, such as an
// upload, whose body the server's ReadTimeout would cut off:
//
//	body, err := edge.Upload(w, r, edge.UploadLimits{Max: 50 << 20})
//	if err != nil {
//		log.Warn("upload without a deadline", "err", err)
//	}
//	r.Body = body
//	if err := r.ParseMultipartForm(1 << 20); edge.TooLarge(err) { ... }
//
// The body gets a Window, and each time it has sent MinRate*Window bytes
// it gets a Window from then, until Max/MinRate+Slack from the call: a slow
// but honest upload takes as long as it needs, while a body that falls
// below MinRate is cut off a Window after the last time it kept up, a
// burst before a stall included. A body cut off fails its Read with an
// error that matches os.ErrDeadlineExceeded, and over HTTP/1.1 the server
// closes the connection after the answer. Reads past Max fail with an
// *http.MaxBytesError (see TooLarge).
//
// The server's WriteTimeout, if it has one, would fail the answer to an
// upload that takes longer, so Upload clears the write deadline while the
// body arrives, and sets it to the server's WriteTimeout from the body's
// end (or its failure), for the answer.
//
// It works with ParseMultipartForm, MultipartReader and plain reads, over
// HTTP/1.1 and HTTP/2. Call it before the first Read. It sets the
// deadlines through http.ResponseController, so w must unwrap to the
// server's writer (lith's Compress and LogRequests do; an app's own
// wrapper needs an Unwrap method returning the writer it wraps). When one
// cannot be set, such as with httptest's recorder, err says so, wrapping
// http.ErrNotSupported, and the body, still capped at Max, is usable
// without it. It sets no read deadline after the body's end or a failed
// read: over HTTP/1.1 Go's server then watches the connection for a client
// that leaves, and a read deadline set on it would cancel the request's
// context when it passed. Clearing it (the zero time, as ReadSignals does)
// is harmless. A write deadline has no such effect.
func Upload(w http.ResponseWriter, r *http.Request, l UploadLimits) (io.ReadCloser, error) {
	l.Max = orDefault(l.Max, UploadMax)
	l.MinRate = orDefault(l.MinRate, UploadMinRate)
	l.Window = orDefault(l.Window, UploadWindow)
	l.Slack = orDefault(l.Slack, UploadSlack)
	now := time.Now()
	u := &uploadBody{
		body:   http.MaxBytesReader(w, r.Body, l.Max),
		rc:     http.NewResponseController(w),
		window: l.Window,
		quota:  max(int64(float64(l.MinRate)*l.Window.Seconds()), 1),
		end:    now.Add(time.Duration(float64(l.Max)/float64(l.MinRate)*float64(time.Second)) + l.Slack),
	}
	if srv, _ := r.Context().Value(http.ServerContextKey).(*http.Server); srv != nil {
		u.writeTimeout = srv.WriteTimeout
	}
	var errs []error
	if err := u.rc.SetReadDeadline(earlier(now.Add(u.window), u.end)); err != nil {
		errs = append(errs, fmt.Errorf("edge: upload read deadline: %w", err))
	} else {
		u.reading = true
	}
	if u.writeTimeout > 0 {
		if err := u.rc.SetWriteDeadline(time.Time{}); err != nil {
			errs = append(errs, fmt.Errorf("edge: upload write deadline: %w", err))
			u.writeTimeout = 0
		}
	}
	return u, errors.Join(errs...)
}

// UploadBody is Upload with a minimum rate and a size limit, and the
// default window and slack.
func UploadBody(w http.ResponseWriter, r *http.Request, minRate, maxSize int64) (io.ReadCloser, error) {
	return Upload(w, r, UploadLimits{Max: maxSize, MinRate: minRate})
}

// TooLarge reports whether err comes from a body over its limit: an
// *http.MaxBytesError, as Upload's body and http.MaxBytesReader return,
// also through ParseMultipartForm, MultipartReader's parts and a
// json.Decoder.
func TooLarge(err error) bool {
	var tooBig *http.MaxBytesError
	return errors.As(err, &tooBig)
}

func orDefault[T int64 | time.Duration](v, def T) T {
	if v > 0 {
		return v
	}
	return def
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

type uploadBody struct {
	body         io.ReadCloser
	rc           *http.ResponseController
	window       time.Duration
	quota        int64     // bytes that earn the next window
	got          int64     // bytes since the last window was earned
	end          time.Time // the latest deadline
	writeTimeout time.Duration
	reading      bool // the read deadline is ours to move, and the body has not ended
	ended        bool
}

func (u *uploadBody) Read(p []byte) (int, error) {
	n, err := u.body.Read(p)
	if u.ended {
		return n, err
	}
	if err != nil {
		// At the end or after a failure the read deadline stays as it is;
		// the answer gets the server's write timeout.
		u.ended, u.reading = true, false
		if u.writeTimeout > 0 {
			_ = u.rc.SetWriteDeadline(time.Now().Add(u.writeTimeout))
		}
		return n, err
	}
	if !u.reading {
		return n, nil
	}
	u.got += int64(n)
	if u.got >= u.quota {
		// The bytes past the quota count for the next window, up to half
		// of one: a steady sender gets no penalty for the size of its
		// reads, a burst buys no stall.
		u.got = min(u.got-u.quota, u.quota/2)
		u.reading = u.rc.SetReadDeadline(earlier(time.Now().Add(u.window), u.end)) == nil
	}
	return n, nil
}

func (u *uploadBody) Close() error { return u.body.Close() }
