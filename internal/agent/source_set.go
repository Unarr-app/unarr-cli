package agent

// SourceSet is the versioned, provider-neutral set of ways an agent can obtain
// a release. Sources sharing a ReleaseKey are transports for the same release;
// content alternatives deliberately carry a different key and Relation.
type SourceSet struct {
	Version           int      `json:"version"`
	PreferredSourceID string   `json:"preferredSourceId,omitempty"`
	Sources           []Source `json:"sources"`
}

// Source is one delivery candidate inside a SourceSet. String fields are used
// for the vocabularies so a newer server can add values without making an older
// agent reject the whole sync response.
type Source struct {
	ID         string `json:"id"`
	ReleaseKey string `json:"releaseKey"`
	Relation   string `json:"relation"`  // exact_release | content_alternative
	Transport  string `json:"transport"` // debrid | usenet | torrent
	Provider   string `json:"provider,omitempty"`
	InfoHash   string `json:"infoHash,omitempty"`
	NzbID      string `json:"nzbId,omitempty"`
	DirectURL  string `json:"directUrl,omitempty"`
	FileName   string `json:"fileName,omitempty"`
	FileSize   int64  `json:"fileSize,omitempty"`
	Password   string `json:"password,omitempty"`
}

// NormalizeSourceSet fills only missing legacy fields from a v1 SourceSet.
//
// During the expand phase the web sends both representations. Keeping the flat
// fields authoritative makes rollback safe and prevents a mixed-version deploy
// from changing routing. The fallback also lets a future web stop duplicating
// the fields once every supported agent understands SourceSet.
func (t *Task) NormalizeSourceSet() {
	if t.SourceSet == nil || t.SourceSet.Version != 1 {
		return
	}

	ordered := preferredSourceFirst(t.SourceSet)
	for _, source := range ordered {
		t.applyLegacySource(source)
	}

	t.normalizePreferredMethod()
}

func (t *Task) applyLegacySource(source Source) {
	if t.InfoHash == "" && source.InfoHash != "" {
		t.InfoHash = source.InfoHash
	}

	switch source.Transport {
	case "debrid":
		if t.DirectURL == "" && source.DirectURL != "" {
			t.DirectURL = source.DirectURL
			t.DirectFileName = source.FileName
			t.DirectFileSize = source.FileSize
		}
	case "usenet":
		if t.NzbID == "" {
			t.NzbID = source.NzbID
		}
		if t.NzbPassword == "" {
			t.NzbPassword = source.Password
		}
	}
}

func (t *Task) normalizePreferredMethod() {
	if t.PreferredMethod != "" {
		return
	}
	preferred := findSource(t.SourceSet.Sources, t.SourceSet.PreferredSourceID)
	if preferred == nil {
		t.PreferredMethod = "auto"
		return
	}
	t.PreferredMethod = preferred.Transport
}

func preferredSourceFirst(sourceSet *SourceSet) []Source {
	preferred := findSource(sourceSet.Sources, sourceSet.PreferredSourceID)
	if preferred == nil {
		return sourceSet.Sources
	}

	ordered := make([]Source, 0, len(sourceSet.Sources))
	ordered = append(ordered, *preferred)
	for i := range sourceSet.Sources {
		if sourceSet.Sources[i].ID != preferred.ID {
			ordered = append(ordered, sourceSet.Sources[i])
		}
	}
	return ordered
}

func findSource(sources []Source, id string) *Source {
	if id == "" {
		return nil
	}
	for i := range sources {
		if sources[i].ID == id {
			return &sources[i]
		}
	}
	return nil
}
