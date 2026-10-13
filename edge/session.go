package edge

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"time"
)

// Sessions hands every browser an opaque session id in a cookie: 256 random
// bits in base64url. It stores nothing, so a crawler's GET costs a Set-Cookie
// header and no row. What a session holds is the app's to store, through its
// writer, once the browser does something worth keeping.
type Sessions struct {
	// Name of the cookie, "sid" when empty. Over HTTPS it gets the __Host-
	// prefix, which binds the cookie to this host and Path=/.
	Name string
	// Secure is for a site served over HTTPS: the cookie is __Host- prefixed
	// and Secure. Without it, in development over plain http, the cookie has
	// its plain name.
	Secure bool
	// MaxAge is the cookie's lifetime, 400 days when zero (the longest
	// browsers keep one).
	MaxAge time.Duration
	// Accept, when set, lets a cookie carry an id that Sessions would not
	// make, such as one in an app's older format, so that a move to Sessions
	// signs no one out. It must check the format: the value is the client's.
	// New ids are 256-bit all the same.
	Accept func(id string) bool
}

const sessionIDLen = 43 // 32 bytes in base64url without padding

type sessionKey struct{}

type sessionValue struct{ name, id string }

func (s Sessions) cookieName() string {
	name := s.Name
	if name == "" {
		name = "sid"
	}
	if s.Secure {
		return "__Host-" + name
	}
	return name
}

// Get returns the session id the request's cookie carries, if it is one.
func (s Sessions) Get(r *http.Request) (string, bool) {
	c, err := r.Cookie(s.cookieName())
	if err != nil || !s.valid(c.Value) {
		return "", false
	}
	return c.Value, true
}

// Ensure returns the request's session id: the one Handler put in its
// context, else its cookie's, else a new one, which it sets as the cookie.
func (s Sessions) Ensure(w http.ResponseWriter, r *http.Request) string {
	if v, ok := r.Context().Value(sessionKey{}).(sessionValue); ok && v.name == s.cookieName() {
		return v.id
	}
	if id, ok := s.Get(r); ok {
		return id
	}
	return s.Renew(w)
}

// Renew sets a new session id and returns it. Call it on login, so that an id
// planted before cannot carry over. The request's context keeps the old one.
func (s Sessions) Renew(w http.ResponseWriter) string {
	b := make([]byte, 32)
	rand.Read(b)
	id := base64.RawURLEncoding.EncodeToString(b)
	maxAge := s.MaxAge
	if maxAge == 0 {
		maxAge = 400 * 24 * time.Hour
	}
	s.set(w, id, int(maxAge/time.Second))
	return id
}

// Clear deletes the cookie, for a logout.
func (s Sessions) Clear(w http.ResponseWriter) {
	s.set(w, "", -1)
}

func (s Sessions) set(w http.ResponseWriter, id string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    id,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// Handler puts the request's session id in its context, for SessionID,
// setting the cookie when the request had none.
func (s Sessions) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := sessionValue{s.cookieName(), s.Ensure(w, r)}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, v)))
	})
}

// SessionID returns the session id Sessions.Handler put in ctx, or "".
func SessionID(ctx context.Context) string {
	v, _ := ctx.Value(sessionKey{}).(sessionValue)
	return v.id
}

func (s Sessions) valid(id string) bool {
	return validSessionID(id) || id != "" && s.Accept != nil && s.Accept(id)
}

func validSessionID(s string) bool {
	if len(s) != sessionIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
