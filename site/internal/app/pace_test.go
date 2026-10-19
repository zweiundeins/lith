package app_test

import (
	"net/http"
	"regexp"
	"strconv"
	"testing"

	"github.com/zweiundeins/lith/site/internal/app/apptest"
)

// TestPacing posts /pace and /ack as a page behind its stream does: /pace
// counts the report and paces the stream, whose next write ends with an
// element that posts /ack. Both reach only the streams of the request's
// session, and both are limited per session.
func TestPacing(t *testing.T) {
	a, base := apptest.Start(t)
	ada, bob := client(t), client(t)
	get(t, ada, base+"/")
	get(t, bob, base+"/")
	events := listen(t, ada, base+"/demos/life/stream?tab=t1")
	freshSoup(t, a)
	next(t, events, `id="life-stats"`)

	if code := post(t, bob, base+"/pace?tab=t1", ""); code != http.StatusNoContent {
		t.Fatalf("pace from another session: %d", code)
	}
	if st := a.Hub().Stats(); st.Paces != 0 || st.Behind != 0 {
		t.Fatalf("another session's /pace: Paces %d, Behind %d", st.Paces, st.Behind)
	}
	if code := post(t, ada, base+"/pace?tab=t1", ""); code != http.StatusNoContent {
		t.Fatalf("pace: %d", code)
	}
	if st := a.Hub().Stats(); st.Paces != 1 || st.Behind != 1 {
		t.Fatalf("Paces %d, Behind %d, want 1 and 1", st.Paces, st.Behind)
	}
	ack := regexp.MustCompile(`/ack\?tab=t1&amp;s=([0-9a-z]+)&amp;n=1&#39; \+ &#39;&amp;rtt=&#39; \+ \(\(x\) =&gt;`)
	ev := next(t, events, `id="ack-1"`)
	m := ack.FindStringSubmatch(ev)
	if m == nil {
		t.Fatalf("no acknowledgement of the write with its round trip matches %s:\n%s", ack, ev)
	}
	url := base + "/ack?tab=t1&s=" + m[1] + "&n=1&rtt=3"
	if code := post(t, bob, url, ""); code != http.StatusNoContent {
		t.Fatalf("ack from another session: %d", code)
	}
	if n := a.Hub().Stats().Acks; n != 0 {
		t.Fatalf("another session's /ack was taken: %d", n)
	}
	if code := post(t, ada, url, ""); code != http.StatusNoContent {
		t.Fatalf("ack: %d", code)
	}
	next(t, events, `id="ack-2"`)
	if n := a.Hub().Stats().Acks; n != 1 {
		t.Errorf("Acks %d, want 1", n)
	}

	for range 80 {
		post(t, ada, url, "")
	}
	if n := a.Hub().Stats().Acks; n < 60 || n > 66 {
		t.Errorf("81 acknowledgements at once: %d taken, want 60 and what refilled", n)
	}
	cleo := client(t)
	get(t, cleo, base+"/")
	for i := range 7 {
		tab := "c" + strconv.Itoa(i)
		next(t, listen(t, cleo, base+"/demos/life/stream?tab="+tab), `id="life-stats"`)
		post(t, cleo, base+"/pace?tab="+tab, "")
	}
	if st := a.Hub().Stats(); st.Paces != 6 {
		t.Errorf("seven reports of one session at once: %d paced, want 5", st.Paces-1)
	}

	for _, url := range []string{"/pace?tab=no%20tab", "/ack?tab=t1&n=1", "/ack?tab=t1&s=X&n=1", "/ack?tab=t1&s=1&n=0", "/ack?tab=t1&s=1&n=1&rtt=x"} {
		if code := post(t, ada, base+url, ""); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", url, code)
		}
	}
}
