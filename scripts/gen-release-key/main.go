// gen-release-key generates an ed25519 keypair for signing release artifacts.
// Run once per repository, then store the printed values:
//
//	RELEASE_SIGNING_KEY     → GitHub Actions secret (private key, base64)
//	RELEASE_SIGNING_PUBKEY  → GitHub Actions variable (public key, base64)
//
// The public key is injected into the binary at build time via the
// goreleaser ldflags entry that resolves
// `github.com/Unarr-app/unarr-cli/internal/upgrade.releasePubKeyBase64`.
// The private key is used by the workflow's "Sign checksums.txt" step.
//
// Build and run:
//
//	go run ./scripts/gen-release-key
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

func main() {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	fmt.Println("# Add the following to your GitHub repository:")
	fmt.Println("#   - Settings → Secrets and variables → Actions → New repository secret")
	fmt.Println("#       RELEASE_SIGNING_KEY  =  <PRIVATE_KEY_BASE64 below>")
	fmt.Println("#   - Settings → Secrets and variables → Actions → New repository variable")
	fmt.Println("#       RELEASE_SIGNING_PUBKEY  =  <PUBLIC_KEY_BASE64 below>")
	fmt.Println()
	fmt.Printf("PUBLIC_KEY_BASE64=%s\n", base64.StdEncoding.EncodeToString(pub))
	fmt.Printf("PRIVATE_KEY_BASE64=%s\n", base64.StdEncoding.EncodeToString(priv))
}
