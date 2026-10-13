// Command minisig checks a minisign signature against a public key given on
// the command line, so the release can be verified without installing minisign.
package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/blake2b"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: minisig <public key> <file> <file.minisig>")
		os.Exit(2)
	}
	if err := verify(os.Args[1], os.Args[2], os.Args[3]); err != nil {
		fmt.Fprintln(os.Stderr, "FAILED:", err)
		os.Exit(1)
	}
}

func verify(key, file, sigFile string) error {
	pk, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(pk) != 42 || string(pk[:2]) != "Ed" {
		return fmt.Errorf("bad public key")
	}
	raw, err := os.ReadFile(sigFile)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) < 4 || !strings.HasPrefix(lines[2], "trusted comment: ") {
		return fmt.Errorf("bad signature file")
	}
	sig, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil || len(sig) != 74 {
		return fmt.Errorf("bad signature line")
	}
	if !bytes.Equal(sig[2:10], pk[2:10]) {
		return fmt.Errorf("key id %X differs from the public key's %X",
			binary.LittleEndian.Uint64(sig[2:10]), binary.LittleEndian.Uint64(pk[2:10]))
	}
	msg, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	switch string(sig[:2]) {
	case "ED":
		h := blake2b.Sum512(msg)
		msg = h[:]
	case "Ed":
	default:
		return fmt.Errorf("unknown algorithm %q", sig[:2])
	}
	pub := ed25519.PublicKey(pk[10:])
	if !ed25519.Verify(pub, msg, sig[10:]) {
		return fmt.Errorf("signature verification failed")
	}
	comment := strings.TrimPrefix(lines[2], "trusted comment: ")
	global, err := base64.StdEncoding.DecodeString(lines[3])
	if err != nil || len(global) != 64 {
		return fmt.Errorf("bad global signature")
	}
	if !ed25519.Verify(pub, append(append([]byte{}, sig[10:]...), comment...), global) {
		return fmt.Errorf("comment signature verification failed")
	}
	fmt.Printf("Signature and comment signature verified (key id %X)\nTrusted comment: %s\n",
		binary.LittleEndian.Uint64(pk[2:10]), comment)
	return nil
}
