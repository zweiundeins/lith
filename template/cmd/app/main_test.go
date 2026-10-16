package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMetrics runs the server with its metrics on a listener of their own.
// After a signature, the metrics listener serves the writer's and the hub's
// numbers under app's namespace, and the public listener has no /metrics.
func TestMetrics(t *testing.T) {
	public, metrics := run(t)
	sign(t, public)
	waitFor(t, "a command and a tick in the metrics", func() bool {
		body := get(t, metrics+"/metrics")
		return value(body, namespace+"_writer_commands_total") >= 1 && value(body, namespace+"_live_ticks_total") >= 1
	})
	res, err := http.Get(public + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound || strings.Contains(string(b), namespace+"_") {
		t.Errorf("the public listener's /metrics: %d\n%.300s", res.StatusCode, b)
	}
}

// TestDashboard checks that every metric of lith/prom and dbmetrics
// (writer_, live_, sse_budget_ and db_), the Go runtime and the process that
// the "lith apps" dashboard and alert rules in lith's deploy/observability
// read is one app exports. Their other rows are an app's own: requests by
// route (http_), and lith/events (events_).
func TestDashboard(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/zweiundeins/lith").Output()
	dir := strings.TrimSpace(string(out))
	if err != nil || dir == "" {
		t.Skipf("lith's files are not at hand (go list: %v)", err)
	}
	public, metrics := run(t)
	sign(t, public) // the budget counts its streams by setting while one is open
	exported := map[string]bool{}
	for line := range strings.Lines(get(t, metrics+"/metrics")) {
		if name, _, ok := strings.Cut(line, "{"); ok && !strings.HasPrefix(line, "#") {
			exported[name] = true
		} else if name, _, ok := strings.Cut(line, " "); ok && !strings.HasPrefix(line, "#") {
			exported[name] = true
		}
	}
	wanted := map[string]string{}
	for _, file := range []string{"deploy/observability/lith-apps.json", "deploy/observability/rules-lith-apps.yaml"} {
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range []*regexp.Regexp{namespaced, anyNamespace} {
			for _, m := range re.FindAllStringSubmatch(string(b), -1) {
				for _, family := range []string{"writer_", "live_", "sse_budget_", "db_"} {
					if strings.HasPrefix(m[1], family) {
						wanted[namespace+"_"+m[1]] = file
					}
				}
			}
		}
		for _, name := range goProcess.FindAllString(string(b), -1) {
			if strings.HasPrefix(name, "go_") || runtime.GOOS == "linux" { // the process collector reads /proc
				wanted[name] = file
			}
		}
	}
	if len(wanted) < 20 {
		t.Fatalf("found only %d metric names in the dashboard and the rules", len(wanted))
	}
	for name, file := range wanted {
		if !exported[name] {
			t.Errorf("%s reads %s, which app does not export", file, name)
		}
	}
}

var (
	namespaced   = regexp.MustCompile(`\$\{ns\}_([a-z0-9_]+)`)
	anyNamespace = regexp.MustCompile(`__name__=~\\?"\.\+_([a-z0-9_]+)\\?"`)
	goProcess    = regexp.MustCompile(`\b(go_[a-z_]+|process_[a-z_]+)\b`)
)

// TestMetricsListen: the metrics listen beside the app, port + 100 on
// loopback, and never on an address the public reaches.
func TestMetricsListen(t *testing.T) {
	for _, c := range []struct{ flag, addr, want string }{
		{"auto", "127.0.0.1:8080", "127.0.0.1:8180"},
		{"auto", "0.0.0.0:8080", "127.0.0.1:8180"},
		{"auto", "unix:/run/app/app.sock", ""},
		{"auto", "127.0.0.1:0", ""},
		{"off", "127.0.0.1:8080", ""},
		{"127.0.0.1:9180", "unix:/run/app/app.sock", "127.0.0.1:9180"},
		{"[::1]:9180", "127.0.0.1:8080", "[::1]:9180"},
	} {
		got, err := metricsListen(c.flag, c.addr)
		if err != nil || got != c.want {
			t.Errorf("-metrics %s with -addr %s: %q, %v; want %q", c.flag, c.addr, got, err, c.want)
		}
	}
	for _, public := range []string{"0.0.0.0:8180", ":8180", "203.0.113.7:8180"} {
		if _, err := metricsListen(public, "127.0.0.1:8080"); err == nil {
			t.Errorf("-metrics %s: accepted", public)
		}
	}
}

// TestMetricsPort checks that the unit's -metrics, the port it may bind and
// the target the host's Prometheus scrapes name one port.
func TestMetricsPort(t *testing.T) {
	unit, err := os.ReadFile("../../deploy/app.service")
	if err != nil {
		t.Fatal(err)
	}
	target, err := os.ReadFile("../../deploy/observability/app.metrics.yml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`\s-metrics 127\.0\.0\.1:(\d+)\s`).FindSubmatch(unit)
	if m == nil {
		t.Fatal("the unit passes no -metrics 127.0.0.1:<port>")
	}
	port := string(m[1])
	if !strings.Contains(string(unit), "\nSocketBindAllow=tcp:"+port+"\n") {
		t.Errorf("the unit serves its metrics on port %s, but may not bind it", port)
	}
	if !strings.Contains(string(target), "- targets: ['127.0.0.1:"+port+"']") {
		t.Errorf("the unit serves its metrics on port %s, but Prometheus scrapes:\n%s", port, target)
	}
}

// run serves app in process, on a free loopback port and its metrics on
// another, until the test ends.
func run(t *testing.T) (public, metrics string) {
	t.Helper()
	addr, maddr := freeAddr(t), freeAddr(t)
	args := []string{"-addr", addr, "-metrics", maddr, "-db", filepath.Join(t.TempDir(), "app.db")}
	ctx, cancel := context.WithCancel(context.Background())
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		err = serve(ctx, args)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		if err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	public, metrics = "http://"+addr, "http://"+maddr
	waitFor(t, "/healthz", func() bool {
		select {
		case <-done:
			t.Fatalf("serve: %v", err)
		default:
		}
		res, err := http.Get(public + "/healthz")
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == http.StatusOK
	})
	return public, metrics
}

// sign opens a tab's stream and signs the guestbook as that tab, then waits
// for the outcome on the stream, which stays open until the test ends.
func sign(t *testing.T, base string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Jar: jar}
	res, err := c.Get(base + "/") // the session's cookie
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/stream?tab=tab1", nil)
	req.Header.Set("Accept-Encoding", "identity")
	stream, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("stream: %d", stream.StatusCode)
	}
	lines := make(chan string)
	go func() {
		defer stream.Body.Close()
		defer close(lines)
		s := bufio.NewScanner(stream.Body)
		s.Buffer(nil, 1<<20)
		for s.Scan() {
			select {
			case lines <- s.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	next := func(want string) {
		t.Helper()
		timeout := time.After(5 * time.Second)
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("the stream ended before a line with %q", want)
				}
				if strings.Contains(line, want) {
					return
				}
			case <-timeout:
				t.Fatalf("no line with %q in 5s", want)
			}
		}
	}
	next(`"_ack":""`)

	req, _ = http.NewRequest("POST", base+"/cmd/sign", strings.NewReader(`{"tab":"tab1","op":"op1","name":"Ada","message":"Hello"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Datastar-Request", "true")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	res, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("sign: %d", res.StatusCode)
	}
	next(`"_ack":"op1","_accepted":true`)
}

func get(t *testing.T, url string) string {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("%s: %d, %v", url, res.StatusCode, err)
	}
	return string(b)
}

// value is the value of the series name without labels in Prometheus' text
// format, or -1.
func value(body, name string) float64 {
	for line := range strings.Lines(body) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), name+" "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err == nil {
				return f
			}
		}
	}
	return -1
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 10 s for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
