package backup

import "errors"

var (
	// ErrBadManifest: manifest.json is missing, unreadable or from a newer schema.
	ErrBadManifest = errors.New("invalid backup manifest")
	// ErrChecksum: a file's size or SHA-256 differs from the manifest.
	ErrChecksum = errors.New("backup checksum mismatch")
	// ErrUnsafePath: an archive member is not on the allowlist or tries to escape it (zip-slip).
	ErrUnsafePath = errors.New("unsafe path in backup archive")
	// ErrTooLarge: the archive or one of its members exceeds the size bounds.
	ErrTooLarge = errors.New("backup archive is too large")
	// ErrBadContent: config.toml does not parse or the queue does not decode as
	// the daemon's task list, so restore refuses it before writing anything — and
	// create before writing an archive restore would refuse.
	ErrBadContent = errors.New("backup content unarr cannot load")
	// ErrNotRedacted: the manifest says the archive has no secrets, yet its
	// config.toml sets one. create never writes that, so the archive was edited
	// afterwards — its checksums cannot tell, anyone can recompute them.
	ErrNotRedacted = errors.New("secret-free backup carries credentials")
	// ErrPassphraseRequired: the archive is encrypted and no passphrase was given.
	ErrPassphraseRequired = errors.New("backup is encrypted: a passphrase is required")
	// ErrBadPassphrase: decryption failed (wrong passphrase or corrupted file).
	ErrBadPassphrase = errors.New("cannot decrypt backup: wrong passphrase or corrupted file")
	// ErrNothingToBackup: none of the allowlisted files exist.
	ErrNothingToBackup = errors.New("nothing to back up: no config or queue file found")
	// ErrRedaction: a secret-free archive was requested but a secret could not
	// be proven gone, so no archive was written.
	ErrRedaction = errors.New("cannot strip secrets from config.toml safely (use --encrypt to back it up whole)")
)
