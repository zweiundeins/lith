package sse

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// MaxSignals is the most bytes of signals ReadSignals takes from a
// request's body.
const MaxSignals = 1 << 20

// SignalsTimeout is how long ReadSignals waits for a request's body: a
// client that declares a body and sends it slowly, or not at all, would
// otherwise hold its connection and a goroutine until it leaves. The
// server's ReadTimeout bounds the body of a request to any route, read or
// not.
const SignalsTimeout = 10 * time.Second

// signalsTimeout is SignalsTimeout, shorter in tests.
var signalsTimeout = SignalsTimeout

// ErrBodyConsumed is returned by ReadSignals when the request's body was
// read before it, as net/http does when Open sends the headers.
var ErrBodyConsumed = errors.New("sse: the request's body was read before ReadSignals: read the signals before Open")

// ReadSignals decodes the Datastar signals of r into v. Datastar sends
// them as the datastar query parameter for GET and DELETE (and ReadSignals
// reads HEAD like GET), and as a JSON body for every other method, @query's
// QUERY included. A missing parameter or an empty body is no signals: v
// stays as it is and the error is nil. A body over MaxSignals bytes fails
// with an *http.MaxBytesError, bad JSON with the decoder's error, and a
// body that has not arrived within SignalsTimeout with an error that
// matches os.ErrDeadlineExceeded: ReadSignals sets a read deadline on the
// connection (http.ResponseController) before it reads the body, and
// clears it once it has the whole body.
//
// ReadSignals reads the body to its end: over HTTP/1.1, net/http notices
// that a client has left only once the body has been read, or discarded
// when the response's headers go out. So call it before Open, and before
// a handler waits on its context with nothing written.
//
// Unlike datastar-go's ReadSignals(r, v), it takes w for
// http.MaxBytesReader, caps the body and accepts an empty one.
func ReadSignals(w http.ResponseWriter, r *http.Request, v any) error {
	return ReadSignalsLimit(w, r, v, MaxSignals)
}

// ReadSignalsLimit is ReadSignals with a limit of its own on the body.
func ReadSignalsLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	var b []byte
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodDelete:
		b = []byte(r.URL.Query().Get("datastar"))
	default:
		if r.Body == nil || r.Body == http.NoBody {
			return nil
		}
		rc := http.NewResponseController(w)
		deadline := rc.SetReadDeadline(time.Now().Add(signalsTimeout)) == nil
		var err error
		b, err = io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
		if deadline && err == nil {
			// After a failed read the deadline stays, so the server closes
			// the connection instead of waiting for the rest of the body.
			rc.SetReadDeadline(time.Time{})
		}
		switch {
		case errors.Is(err, http.ErrBodyReadAfterClose), err == nil && len(b) == 0 && r.ContentLength > 0:
			return ErrBodyConsumed
		case err != nil:
			return fmt.Errorf("sse: reading signals: %w", err)
		}
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("sse: decoding signals: %w", err)
	}
	return nil
}
