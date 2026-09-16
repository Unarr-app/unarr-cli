package engine

import (
	"fmt"
	"path"
	"strings"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// NextDebridRepairSource returns and reserves the next unresolved debrid
// candidate. Reserving before the network call prevents one bad provider from
// being retried forever; a later task re-dispatch receives a fresh SourceSet.
func (t *Task) NextDebridRepairSource() (agent.Source, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.SourceSet == nil || t.SourceSet.Version != 1 {
		return agent.Source{}, false
	}
	if t.triedSourceIDs == nil {
		t.triedSourceIDs = make(map[string]struct{})
	}

	for _, source := range t.SourceSet.Sources {
		if source.Transport != "debrid" || source.Relation != "exact_release" ||
			source.Provider == "" || source.DirectURL != "" {
			continue
		}
		if _, tried := t.triedSourceIDs[source.ID]; tried {
			continue
		}
		t.triedSourceIDs[source.ID] = struct{}{}
		return source, true
	}
	return agent.Source{}, false
}

// ApplyDebridRepairSource atomically swaps only the transport URL after proving
// the candidate still names the same release and file. Existing filename/size
// remain authoritative so a provider cannot redirect a partial onto new bytes.
func (t *Task) ApplyDebridRepairSource(source agent.Source) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if source.Transport != "debrid" || source.Relation != "exact_release" || source.DirectURL == "" {
		return fmt.Errorf("invalid debrid repair source %q", source.ID)
	}
	if t.InfoHash == "" || !strings.EqualFold(source.InfoHash, t.InfoHash) {
		return fmt.Errorf("repair source %q changed release identity", source.ID)
	}
	if t.DirectFileName != "" && source.FileName != "" &&
		portableBase(t.DirectFileName) != portableBase(source.FileName) {
		return fmt.Errorf("repair source %q changed file from %q to %q", source.ID, t.DirectFileName, source.FileName)
	}
	if t.DirectFileSize > 0 && source.FileSize > 0 && t.DirectFileSize != source.FileSize {
		return fmt.Errorf("repair source %q changed file size from %d to %d", source.ID, t.DirectFileSize, source.FileSize)
	}

	t.DirectURL = source.DirectURL
	if t.DirectFileName == "" {
		t.DirectFileName = source.FileName
	}
	if t.DirectFileSize == 0 {
		t.DirectFileSize = source.FileSize
	}
	return nil
}

func portableBase(name string) string {
	return path.Base(strings.ReplaceAll(name, "\\", "/"))
}
