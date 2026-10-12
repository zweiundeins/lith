package edge

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func cookies(w *httptest.ResponseRecorder) []*http.Cookie {
	return (&http.Response{Header: w.Header()}).Cookies()
}

func TestSessionCookieAttributes(t *testing.T) {
	for _, c := range []struct {
		s      Sessions
		name   string
		secure bool
	}{
		{Sessions{Secure: true}, "__Host-sid", true},
		{Sessions{}, "sid", false},
		{Sessions{Name: "member", Secure: true}, "__Host-member", true},
	} {
		w := httptest.NewRecorder()
		id := c.s.Ensure(w, httptest.NewRequest("GET", "/", nil))
		got := cookies(w)
		if len(got) != 1 {
			t.Fatalf("%s: %d cookies set, want 1", c.name, len(got))
		}
		ck := got[0]
		if ck.Name != c.name || ck.Value != id || ck.Secure != c.secure || !ck.HttpOnly ||
			ck.SameSite != http.SameSiteLaxMode || ck.Path != "/" || ck.Domain != "" ||
			ck.MaxAge != int((400*24*time.Hour).Seconds()) {
			t.Errorf("%s: cookie %q", c.name, w.Header().Get("Set-Cookie"))
		}
		if len(id) != 43 || strings.Trim(id, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
			t.Errorf("%s: id %q is not 32 bytes of base64url", c.name, id)
		}
	}
}

func TestSessionGet(t *testing.T) {
	s := Sessions{Secure: true}
	valid := strings.Repeat("a", 43)
	for _, c := range []struct {
		cookie string
		ok     bool
	}{
		{"__Host-sid=" + valid, true},
		{"sid=" + valid, false}, // the dev name does not count over HTTPS
		{"__Host-sid=" + valid[:42], false},
		{"__Host-sid=" + valid + "a", false},
		{"__Host-sid=" + valid[:42] + "+", false},
		{"__Host-sid=", false},
		{"", false},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		if c.cookie != "" {
			r.Header.Set("Cookie", c.cookie)
		}
		if id, ok := s.Get(r); ok != c.ok || (ok && id != valid) {
			t.Errorf("Cookie %q: Get = %q, %v", c.cookie, id, ok)
		}
	}
}

func TestSessionEnsureKeepsAnExistingID(t *testing.T) {
	s := Sessions{}
	w := httptest.NewRecorder()
	first := s.Ensure(w, httptest.NewRequest("GET", "/", nil))

	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: "sid", Value: first})
	w = httptest.NewRecorder()
	if id := s.Ensure(w, r); id != first {
		t.Errorf("Ensure = %q, want the cookie's %q", id, first)
	}
	if h := w.Header().Get("Set-Cookie"); h != "" {
		t.Errorf("Ensure set a cookie the request already had: %q", h)
	}
}

func TestSessionHandler(t *testing.T) {
	s := Sessions{Secure: true}
	var inCtx, again string
	h := s.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inCtx = SessionID(r.Context())
		again = s.Ensure(w, r)
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	got := cookies(w)
	if len(got) != 1 || got[0].Value != inCtx || inCtx == "" || again != inCtx {
		t.Errorf("one cookie for the context's id expected: cookies %d, ctx %q, Ensure %q", len(got), inCtx, again)
	}

	// A second manager with another cookie keeps its own id.
	member := Sessions{Name: "member", Secure: true}
	var memberID string
	h = s.Handler(member.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		memberID = member.Ensure(w, r)
		inCtx = SessionID(r.Context())
	})))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if got := cookies(w); len(got) != 2 || memberID != inCtx || got[1].Name != "__Host-member" || got[1].Value != memberID || got[0].Value == memberID {
		t.Errorf("nested managers mixed up their ids: %q", w.Header().Values("Set-Cookie"))
	}
}

func TestSessionRenewAndClear(t *testing.T) {
	s := Sessions{Secure: true}
	w := httptest.NewRecorder()
	a := s.Renew(w)
	b := s.Renew(w)
	if a == b {
		t.Error("Renew returned the same id twice")
	}
	w = httptest.NewRecorder()
	s.Clear(w)
	got := cookies(w)
	if len(got) != 1 || got[0].Name != "__Host-sid" || got[0].Value != "" || got[0].MaxAge >= 0 || !got[0].Secure {
		t.Errorf("Clear: %q", w.Header().Get("Set-Cookie"))
	}
}
