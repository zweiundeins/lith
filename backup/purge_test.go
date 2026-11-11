package backup

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// laggingS3 is gofakes3 behind a handler whose object listings lag, as
// OpenStack Swift's can: it leaves a level-0 file out of the next hide
// listings, and lists ghost, an object that is not there, in every listing
// after the next skip.
type laggingS3 struct {
	mu    sync.Mutex
	hide  int
	hid   []string
	ghost string
	skip  int
}

var level0Entry = regexp.MustCompile(`(?s)<Contents>\s*<Key>([^<]*/0000/[^<]*)</Key>.*?</Contents>`)

func (l *laggingS3) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("list-type") != "2" {
			next.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		body := rec.Body.String()
		l.mu.Lock()
		if l.hide > 0 {
			l.hide--
			if m := level0Entry.FindStringSubmatchIndex(body); m != nil {
				l.hid = append(l.hid, body[m[2]:m[3]])
				body = body[:m[0]] + body[m[1]:]
			}
		}
		if l.ghost != "" {
			if l.skip > 0 {
				l.skip--
			} else {
				body = strings.Replace(body, "</ListBucketResult>", "<Contents><Key>"+l.ghost+"</Key><Size>1</Size></Contents></ListBucketResult>", 1)
			}
		}
		l.mu.Unlock()
		for k, v := range rec.Header() {
			if k != "Content-Length" {
				w.Header()[k] = v
			}
		}
		w.WriteHeader(rec.Code)
		_, _ = io.WriteString(w, body)
	})
}

// lagged replicates a database to gofakes3 behind l and detaches it.
func lagged(t *testing.T, l *laggingS3) *Manager {
	t.Helper()
	srv := httptest.NewServer(l.wrap(fakeS3(t)))
	t.Cleanup(srv.Close)
	m, err := New(Config{Bucket: "lith", Endpoint: srv.URL, Region: "us-east-1", ForcePathStyle: true, AccessKeyID: "test", SecretAccessKey: "test", PurgeSettle: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
	writeAndWaitLevel0(t, m, db, "app", 1)
	shutdown(t, db, detach)
	return m
}

// A purge deletes the object its own listing left out, which a later
// listing shows.
func TestPurgeDeletesWhatALaggingListingMissed(t *testing.T) {
	l := &laggingS3{}
	m := lagged(t, l)
	l.mu.Lock()
	l.hide = 1 // the listing of litestream's DeleteAll
	l.mu.Unlock()
	if err := m.PurgeReplica(bg, "app"); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	hid := l.hid
	l.mu.Unlock()
	if len(hid) != 1 {
		t.Fatalf("hid %v from the purge's listing, want one level-0 file", hid)
	}
	api, err := m.multipart(bg)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := listKeys(bg, api.(objectsAPI), aws.String("lith"), "app/")
	if err != nil || len(keys) != 0 {
		t.Fatalf("left in the bucket: %v, %v", keys, err)
	}
}

// A purge fails while its listings still show an object after the deletes,
// so that the caller purges again.
func TestPurgeFailsWhileObjectsStayListed(t *testing.T) {
	l := &laggingS3{}
	m := lagged(t, l)
	l.mu.Lock()
	l.ghost, l.skip = "app/0000/ghost.ltx", 1 // shown after DeleteAll's listing
	l.mu.Unlock()
	err := m.PurgeReplica(bg, "app")
	if err == nil || !strings.Contains(err.Error(), "1 objects still listed") {
		t.Fatalf("PurgeReplica = %v, want objects still listed", err)
	}
}
