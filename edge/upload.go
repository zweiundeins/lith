package edge

import (
	"io"
	"net/http"
	"time"
)

// UploadWindow is the time an upload's body gets at first, and each time it
// earns more: UploadBody's window.
const UploadWindow = 10 * time.Second

// UploadMinRate is a minimum rate for UploadBody, in bytes a second, for an
// upload over a slow link: three uploads sharing a 0.4 Mbit/s mobile link
// get about 133 kbit/s each, some 16 KB/s, twice its 8 KiB/s, while a
// client that trickles a byte a second is cut off after a window.
const UploadMinRate = 8 << 10

// UploadBody wraps r.Body for a handler that takes a large body, such as an
// upload, whose body the server's ReadTimeout would cut off:
//
//	r.Body = edge.UploadBody(w, r, edge.UploadMinRate, 10<<20)
//	if err := r.ParseMultipartForm(1 << 20); err != nil { ... }
//
// The body gets UploadWindow to send minRate*UploadWindow bytes, and one
// more window each time it has, never more than one ahead, until
// maxSize/minRate from the call at the latest (at least one window): a
// slow but honest upload takes as long as it needs, while a body that
// falls below minRate is cut off after a window or two, a burst before a
// stall included. Reads past maxSize fail with an *http.MaxBytesError
// (http.MaxBytesReader). A body cut off fails its Read with an error that matches
// os.ErrDeadlineExceeded, and over HTTP/1.1 the server closes the
// connection after the answer.
//
// It works with ParseMultipartForm, MultipartReader and plain reads, over
// HTTP/1.1 and HTTP/2. Call it before the first Read: it sets the read
// deadline through http.ResponseController, so w must unwrap to the
// server's writer (lith's Compress and LogRequests do); with one that
// does not, such as httptest's recorder, only the size limit holds. It
// never touches the deadline after the body's end or a failed read: over
// HTTP/1.1 Go's server then watches the connection for a client that
// leaves, and a deadline set on it would cancel the request's context
// when it passed.
//
// minRate and maxSize of 0 or less mean UploadMinRate and 10 MiB.
func UploadBody(w http.ResponseWriter, r *http.Request, minRate, maxSize int64) io.ReadCloser {
	if minRate <= 0 {
		minRate = UploadMinRate
	}
	if maxSize <= 0 {
		maxSize = 10 << 20
	}
	now := time.Now()
	total := max(time.Duration(float64(maxSize)/float64(minRate)*float64(time.Second)), uploadWindow)
	u := &uploadBody{
		body:   http.MaxBytesReader(w, r.Body, maxSize),
		rc:     http.NewResponseController(w),
		window: uploadWindow,
		quota:  max(int64(float64(minRate)*uploadWindow.Seconds()), 1),
		end:    now.Add(total),
	}
	u.deadline = earlier(now.Add(u.window), u.end)
	u.ok = u.rc.SetReadDeadline(u.deadline) == nil
	return u
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// uploadWindow is UploadWindow, shorter in tests.
var uploadWindow = UploadWindow

type uploadBody struct {
	body     io.ReadCloser
	rc       *http.ResponseController
	window   time.Duration
	quota    int64     // bytes that earn the next window
	got      int64     // bytes since the last window was earned
	deadline time.Time // the read deadline set
	end      time.Time // the latest deadline
	ok       bool      // the writer takes deadlines, and the body has not ended
}

func (u *uploadBody) Read(p []byte) (int, error) {
	n, err := u.body.Read(p)
	if err != nil {
		u.ok = false // at the end or after a failure, the deadline stays as it is
		return n, err
	}
	if !u.ok {
		return n, nil
	}
	u.got += int64(n)
	// One window at a time: a burst earns no more than the next one.
	if u.got >= u.quota && u.deadline.Before(u.end) && time.Until(u.deadline) <= u.window {
		u.got = 0
		u.deadline = earlier(u.deadline.Add(u.window), u.end)
		u.ok = u.rc.SetReadDeadline(u.deadline) == nil
	}
	return n, nil
}

func (u *uploadBody) Close() error { return u.body.Close() }
