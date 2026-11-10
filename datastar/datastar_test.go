package datastar

import (
	"bytes"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestVersion checks that Version and VERSION describe the embedded build:
// its release, the Starbase commit it came from, its size and its hash.
func TestVersion(t *testing.T) {
	js := Datastar()
	release, commit, _ := strings.Cut(Version, "+starbase.")
	if !bytes.HasPrefix(js, []byte("// Datastar v"+release+" + Rocket")) {
		t.Errorf("Datastar() starts with %q, want Datastar v%s", js[:min(len(js), 60)], release)
	}
	version, err := os.ReadFile("VERSION")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha512.Sum384(js)
	line := fmt.Sprintf("datastar-rocket.js\t%d bytes\tsha384-%s", len(js), base64.StdEncoding.EncodeToString(sum[:]))
	if commit == "" || !bytes.Contains(version, []byte("at commit "+commit)) || !bytes.Contains(version, []byte(line)) {
		t.Errorf("VERSION should name commit %q and %s:\n%s", commit, line, version)
	}
	js[0] = 'x'
	if Datastar()[0] != '/' {
		t.Error("Datastar returned the bytes it keeps")
	}
}

// TestIntegrity checks Integrity against the embedded bytes.
func TestIntegrity(t *testing.T) {
	sum := sha512.Sum384(Datastar())
	if got := "sha384-" + base64.StdEncoding.EncodeToString(sum[:]); got != Integrity {
		t.Errorf("the build's integrity is %s, Integrity says %s", got, Integrity)
	}
}
