package arr

import (
	"strconv"
	"strings"
	"time"
)

// IndexerFlags is a numeric bit enum and it is NOT the same in Sonarr and
// Radarr (Internal = 8 vs 32; in Radarr 8 is "PTP Golden"). Values verified
// against Sonarr 4.0.20 and Radarr 6.4.4.
const (
	flagFreeleech      = 1
	sonarrFlagInternal = 8
	radarrFlagInternal = 32
)

// InternalFlag returns the "Internal" IndexerFlags bit for the app, or 0 for
// an app that has no per-file indexer flags (Prowlarr).
func InternalFlag(app string) int {
	switch app {
	case "sonarr":
		return sonarrFlagInternal
	case "radarr":
		return radarrFlagInternal
	}
	return 0
}

// MediaFile is an imported episode/movie file with the flags *arr scores it by.
// IndexerFlags is a pointer so a version that does not expose it is detectable.
type MediaFile struct {
	ID           int  `json:"id"`
	IndexerFlags *int `json:"indexerFlags"`
}

// TrueSpecState is what TorrentClaw knows about one infohash.
type TrueSpecState struct {
	Verified bool
	Mismatch bool
}

// FlagChange is one file whose IndexerFlags must be rewritten.
type FlagChange struct {
	ID       int    `json:"id"`
	Flags    int    `json:"indexerFlags"`
	Old      int    `json:"-"`
	Hash     string `json:"-"`
	Verified bool   `json:"-"`
	FromTC   bool   `json:"-"`
}

// FlagInputs is everything the planner needs; it performs no I/O.
type FlagInputs struct {
	App string
	// Files are the current state of the imported files in *arr.
	Files []MediaFile
	// HashOf maps file id → lowercase infohash of the download that created it.
	HashOf map[int]string
	// IndexerOf maps lowercase infohash → indexer name of its Grabbed event.
	IndexerOf map[string]string
	// TrueSpec maps lowercase infohash → TorrentClaw's state. A hash missing
	// from the map is unknown and its file is left untouched.
	TrueSpec map[string]TrueSpecState
	// TCMatch is matched (case-insensitively) against the grab's indexer name
	// to decide whether the release came from TorrentClaw.
	TCMatch string
}

// PlanFlagChanges returns the files whose flags differ from what TrueSpec says:
// Internal follows the verification; a grab that came from TorrentClaw also
// loses the Freeleech bit the old feed (downloadvolumefactor=0) put on every
// release. Files whose hash TorrentClaw does not know are never touched.
func PlanFlagChanges(in FlagInputs) []FlagChange {
	internal := InternalFlag(in.App)
	if internal == 0 {
		return nil
	}
	var out []FlagChange
	for _, f := range in.Files {
		if f.IndexerFlags == nil {
			continue
		}
		if c, ok := planOne(f, internal, in); ok {
			out = append(out, c)
		}
	}
	return out
}

func planOne(f MediaFile, internal int, in FlagInputs) (FlagChange, bool) {
	hash := in.HashOf[f.ID]
	state, known := in.TrueSpec[hash]
	if hash == "" || !known {
		return FlagChange{}, false
	}
	old := *f.IndexerFlags
	fromTC := in.TCMatch != "" &&
		strings.Contains(strings.ToLower(in.IndexerOf[hash]), strings.ToLower(in.TCMatch))

	next := old
	switch {
	case state.Verified:
		next |= internal
	case fromTC:
		next &^= internal
	}
	if fromTC {
		next &^= flagFreeleech
	}
	if next == old {
		return FlagChange{}, false
	}
	return FlagChange{ID: f.ID, Flags: next, Old: old, Hash: hash, Verified: state.Verified, FromTC: fromTC}, true
}

// LatestImports maps file id → lowercase infohash from downloadFolderImported
// events. Events without a valid 40-hex download id (manual imports) or older
// than since (when non-zero) are skipped; the newest event wins per file.
func LatestImports(records []HistoryRecord, since time.Time) map[int]string {
	type seen struct {
		hash string
		at   time.Time
	}
	latest := map[int]seen{}
	for _, r := range records {
		id, err := strconv.Atoi(r.Data.FileID)
		hash := strings.ToLower(r.DownloadID)
		if err != nil || !isInfoHash(hash) || (!since.IsZero() && r.Date.Before(since)) {
			continue
		}
		if cur, ok := latest[id]; !ok || r.Date.After(cur.at) {
			latest[id] = seen{hash: hash, at: r.Date}
		}
	}
	out := make(map[int]string, len(latest))
	for id, s := range latest {
		out[id] = s.hash
	}
	return out
}

// GrabIndexers maps lowercase infohash → indexer name from grabbed events.
func GrabIndexers(records []HistoryRecord) map[string]string {
	out := map[string]string{}
	for _, r := range records {
		if h := strings.ToLower(r.DownloadID); isInfoHash(h) && r.Data.Indexer != "" {
			out[h] = r.Data.Indexer
		}
	}
	return out
}

func isInfoHash(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
