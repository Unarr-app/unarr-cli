// sign-checksums signs the dist/checksums.txt file with an ed25519 private
// key and writes the base64-encoded signature to the path given by -out.
//
// Usage (from release workflow):
//
//	go run ./scripts/sign-checksums \
//	    -key "$RELEASE_SIGNING_KEY" \
//	    -in  dist/checksums.txt \
//	    -out dist/checksums.txt.sig
//
// The companion CLI verifier (internal/upgrade/signature.go) requires the
// signature to be base64 text, so emitting base64 + trailing newline makes
// the artifact safe to inspect with `cat` / the GitHub release UI.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
)

func main() {
	keyB64 := flag.String("key", "", "base64-encoded ed25519 private key (PrivateKeySize = 64 bytes)")
	in := flag.String("in", "", "path to file to sign")
	out := flag.String("out", "", "path to write the base64-encoded signature")
	flag.Parse()

	if *keyB64 == "" || *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: sign-checksums -key <base64> -in <path> -out <path>")
		os.Exit(2)
	}

	keyBytes, err := base64.StdEncoding.DecodeString(*keyB64)
	if err != nil {
		fail("decode key: %v", err)
	}
	if len(keyBytes) != ed25519.PrivateKeySize {
		fail("private key size %d, expected %d", len(keyBytes), ed25519.PrivateKeySize)
	}
	priv := ed25519.PrivateKey(keyBytes)

	content, err := os.ReadFile(*in)
	if err != nil {
		fail("read input: %v", err)
	}

	sig := ed25519.Sign(priv, content)
	encoded := base64.StdEncoding.EncodeToString(sig) + "\n"
	if err := os.WriteFile(*out, []byte(encoded), 0o644); err != nil {
		fail("write signature: %v", err)
	}
	fmt.Printf("Signed %s (%d bytes) → %s\n", *in, len(content), *out)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
