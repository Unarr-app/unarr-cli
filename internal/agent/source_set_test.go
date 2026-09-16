package agent

import (
	"encoding/json"
	"testing"
)

func TestTaskNormalizeSourceSet(t *testing.T) {
	var task Task
	err := json.Unmarshal([]byte(`{
		"id":"task-1",
		"sourceSet":{
			"version":1,
			"preferredSourceId":"debrid-rd",
			"sources":[
				{"id":"torrent","releaseKey":"btih:abc","relation":"exact_release","transport":"torrent","infoHash":"abc"},
				{"id":"debrid-rd","releaseKey":"btih:abc","relation":"exact_release","transport":"debrid","provider":"real-debrid","infoHash":"abc","directUrl":"https://cdn.example/movie.mkv","fileName":"Movie.mkv","fileSize":1234}
			]
		}
	}`), &task)
	if err != nil {
		t.Fatalf("decode task: %v", err)
	}

	task.NormalizeSourceSet()

	if task.PreferredMethod != "debrid" {
		t.Fatalf("PreferredMethod = %q, want debrid", task.PreferredMethod)
	}
	if task.InfoHash != "abc" {
		t.Errorf("InfoHash = %q, want abc", task.InfoHash)
	}
	if task.DirectURL != "https://cdn.example/movie.mkv" {
		t.Errorf("DirectURL = %q", task.DirectURL)
	}
	if task.DirectFileName != "Movie.mkv" || task.DirectFileSize != 1234 {
		t.Errorf("direct file = (%q, %d)", task.DirectFileName, task.DirectFileSize)
	}
}

func TestTaskNormalizeSourceSetKeepsLegacyFieldsAuthoritative(t *testing.T) {
	task := Task{
		InfoHash:        "legacy-hash",
		PreferredMethod: "torrent",
		DirectURL:       "https://legacy.example/file.mkv",
		DirectFileName:  "Legacy.mkv",
		DirectFileSize:  99,
		NzbID:           "legacy-nzb",
		NzbPassword:     "legacy-password",
		SourceSet: &SourceSet{
			Version:           1,
			PreferredSourceID: "debrid",
			Sources: []Source{
				{
					ID: "debrid", Transport: "debrid", InfoHash: "new-hash",
					DirectURL: "https://new.example/file.mkv", FileName: "New.mkv", FileSize: 100,
				},
				{ID: "usenet", Transport: "usenet", NzbID: "new-nzb", Password: "new-password"},
			},
		},
	}

	task.NormalizeSourceSet()

	if task.InfoHash != "legacy-hash" || task.PreferredMethod != "torrent" {
		t.Errorf("legacy routing changed: %+v", task)
	}
	if task.DirectURL != "https://legacy.example/file.mkv" || task.DirectFileName != "Legacy.mkv" || task.DirectFileSize != 99 {
		t.Errorf("legacy direct source changed: %+v", task)
	}
	if task.NzbID != "legacy-nzb" || task.NzbPassword != "legacy-password" {
		t.Errorf("legacy usenet source changed: %+v", task)
	}
}

func TestTaskNormalizeSourceSetIgnoresUnknownVersion(t *testing.T) {
	task := Task{
		SourceSet: &SourceSet{
			Version: 2,
			Sources: []Source{{Transport: "torrent", InfoHash: "future"}},
		},
	}

	task.NormalizeSourceSet()

	if task.InfoHash != "" || task.PreferredMethod != "" {
		t.Fatalf("unknown SourceSet version changed legacy fields: %+v", task)
	}
}
