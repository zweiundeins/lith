package sse

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type signals struct {
	Tab   string `json:"_tab"`
	Query string `json:"query"`
}

func TestReadSignals(t *testing.T) {
	param := "/?datastar=" + url.QueryEscape(`{"_tab":"t1","query":"lith"}`)
	body := `{"_tab":"t1","query":"lith"}`
	for _, c := range []struct {
		name, method, target, body string
		want                       signals
	}{
		{"GET with the parameter", "GET", param, "", signals{"t1", "lith"}},
		{"GET without", "GET", "/?tab=t1", "", signals{"kept", ""}},
		{"DELETE with the parameter", "DELETE", param, "", signals{"t1", "lith"}},
		{"POST", "POST", "/", body, signals{"t1", "lith"}},
		{"QUERY", "QUERY", "/", body, signals{"t1", "lith"}},
		{"POST without a body", "POST", "/", "", signals{"kept", ""}},
		{"POST with spaces", "POST", "/", " \n", signals{"kept", ""}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var b io.Reader
			if c.body != "" {
				b = strings.NewReader(c.body)
			}
			r := httptest.NewRequest(c.method, c.target, b)
			got := signals{Tab: "kept"}
			if err := ReadSignals(httptest.NewRecorder(), r, &got); err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestReadSignalsRefuses(t *testing.T) {
	var v signals
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"query":"`+strings.Repeat("x", MaxSignals)+`"}`))
	var tooLarge *http.MaxBytesError
	if err := ReadSignals(httptest.NewRecorder(), r, &v); !errors.As(err, &tooLarge) {
		t.Errorf("oversized body: %v", err)
	}
	r = httptest.NewRequest("POST", "/", strings.NewReader(`{"query":"lith"}`))
	if err := ReadSignalsLimit(httptest.NewRecorder(), r, &v, 8); !errors.As(err, &tooLarge) {
		t.Errorf("body over its own limit: %v", err)
	}
	for _, r := range []*http.Request{
		httptest.NewRequest("POST", "/", strings.NewReader(`{"query":`)),
		httptest.NewRequest("QUERY", "/", strings.NewReader(`{"query":"a"} trailing`)),
		httptest.NewRequest("GET", "/?datastar=%7Bnope", nil),
	} {
		var syntax *json.SyntaxError
		if err := ReadSignals(httptest.NewRecorder(), r, &v); !errors.As(err, &syntax) {
			t.Errorf("%s with bad JSON: %v", r.Method, err)
		}
	}
}

// leaves sends a QUERY with a body over HTTP/1.1 to a handler that calls
// read, closes the connection once the handler has called it, and reports
// whether the handler's context ended within wait.
func leaves(t *testing.T, read func(w http.ResponseWriter, r *http.Request), wait time.Duration) bool {
	t.Helper()
	ready := make(chan struct{})
	ended := make(chan bool, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		read(w, r)
		close(ready)
		select {
		case <-r.Context().Done():
			ended <- true
		case <-time.After(wait):
			ended <- false
		}
	}))
	defer srv.Close()

	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	body := `{"query":"lith"}`
	fmt.Fprintf(c, "QUERY / HTTP/1.1\r\nHost: lith\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	<-ready
	c.Close()
	return <-ended
}

// A handler that has read the body hears the client leave at once.
func TestReadSignalsLetsDisconnectsThrough(t *testing.T) {
	ok := leaves(t, func(w http.ResponseWriter, r *http.Request) {
		var v signals
		if err := ReadSignals(w, r, &v); err != nil {
			t.Error(err)
		}
	}, time.Second)
	if !ok {
		t.Error("the context lived on after the client left")
	}

	// Without the read, before any response, it would not: net/http
	// watches the connection only after the body's end.
	if leaves(t, func(w http.ResponseWriter, r *http.Request) {}, 300*time.Millisecond) {
		t.Error("net/http noticed a client that left before its body was read: ReadSignals' doc comment is out of date")
	}
}

// A stream that never read its signals still ends when the client leaves:
// net/http discards a small body when the stream's headers go out.
func TestOpenLetsDisconnectsThrough(t *testing.T) {
	ok := leaves(t, func(w http.ResponseWriter, r *http.Request) {
		s, err := Open(w, r, Options{NoCompression: true})
		if err != nil {
			t.Error(err)
			return
		}
		go func() {
			<-r.Context().Done()
			s.Close()
		}()
	}, time.Second)
	if !ok {
		t.Error("the stream's context lived on after the client left")
	}
}

// Once Open has sent the headers, net/http has discarded the body: ReadSignals
// says so instead of decoding no signals.
func TestReadSignalsAfterOpen(t *testing.T) {
	got := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, err := Open(w, r, Options{NoCompression: true})
		if err != nil {
			got <- err
			return
		}
		defer s.Close()
		var v signals
		got <- ReadSignals(w, r, &v)
	}))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(t.Context(), "QUERY", srv.URL, strings.NewReader(`{"query":"lith"}`))
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if err := <-got; !errors.Is(err, ErrBodyConsumed) {
		t.Errorf("got %v, want ErrBodyConsumed", err)
	}
}
