package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Unarr-app/unarr-cli/internal/agent"
	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/support"
)

// fillStrings sets every string leaf reachable from v to a distinct sentinel.
// The counter leads, so no two sentinels share the 8-character prefix the
// scrubber also erases (a common prefix would make every field look secret).
func fillStrings(v reflect.Value, path string, n *int) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		fillStrings(v.Elem(), path, n)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if f := v.Type().Field(i); f.IsExported() {
				fillStrings(v.Field(i), path+"-"+f.Name, n)
			}
		}
	case reflect.String:
		*n++
		v.SetString(fmt.Sprintf("v%03d%s", *n, path))
	default:
	}
}

// TestRedactedConfigCarriesNoSupportSecret ties the backup's secret list to the
// support bundle's classification (internal/support configFields), the
// enforced inventory of every config field. Every string field gets a distinct
// value; the redacted config is then run through support's scrubber, which
// rewrites any Secret-classified value it finds. A new credential field that is
// classified there but not blanked here turns this test red.
func TestRedactedConfigCarriesNoSupportSecret(t *testing.T) {
	cfg := config.Default()
	n := 0
	fillStrings(reflect.ValueOf(&cfg).Elem(), "", &n)
	// The one deliberate exception (see configSecrets): a local mount point.
	cfg.Mount.Directory = ""
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := config.Save(cfg, p); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	redacted, err := redactConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if scrubbed := support.NewScrubber(cfg).Text(redacted); string(scrubbed) != string(redacted) {
		t.Fatalf("the redacted config still holds a Secret-classified value:\n--- redacted\n%s\n--- scrubbed\n%s", redacted, scrubbed)
	}
	// Control: the scrubber does see the secrets in the original.
	if string(support.NewScrubber(cfg).Text(raw)) == string(raw) {
		t.Fatal("scrubber found nothing in the unredacted config: the guard above proves nothing")
	}
}

// Valid TOML the line rewrite does not understand must be REFUSED, never
// archived with the secret inside (nor rewritten into broken TOML).
func TestRedactionFailsClosed(t *testing.T) {
	for _, cfg := range []string{
		"auth.api_key = \"S3CRET-1\"\n",
		"auth = { api_key = \"S3CRET-2\" }\n",
		"[auth]\n\"api_key\" = \"S3CRET-3\"\n",
		"[auth]\napi_key = \"\"\"S3CRET-4\"\"\"\n",
		"[downloads]\nwebdav_password = '''S3CRET-5'''\n",
		"[agent]\nagent_hash = \"\"\"\nS3CRET-6\"\"\"\n",
	} { // a config that does not parse at all is ErrBadContent: see content_test.go
		l := Layout{ConfigPath: filepath.Join(t.TempDir(), "config.toml"), DataDir: t.TempDir()}
		write(t, l.ConfigPath, cfg)
		dir := t.TempDir()
		_, err := Create(CreateOptions{Layout: l, Dir: dir})
		if !errors.Is(err, ErrRedaction) {
			t.Errorf("%q: err = %v, want ErrRedaction", cfg, err)
		}
		if es, _ := os.ReadDir(dir); len(es) != 0 {
			t.Errorf("%q: an archive was written despite the refusal", cfg)
		}
	}
}

// A plain archive's checksums prove nothing about who wrote it: anyone can edit
// config.toml and recompute them. One whose manifest says it has no secrets but
// whose config sets one was edited after create (which proves them blank), and
// restoring it let the archive's api_key replace this machine's while the plan
// said "the credentials already on this machine are kept". It is refused before
// anything is written — on a machine with credentials and on a fresh one.
func TestRestoreRefusesRedactedArchiveCarryingSecrets(t *testing.T) {
	for _, cfg := range []string{
		"[auth]\napi_key = \"ATTACKER\"\n",
		"[agent]\nid = \"agent-1\"\nagent_hash = \"ATTACKER\"\n",
		"[downloads]\nwebdav_username = \"ATTACKER\"\n",
		"[downloads]\nwebdav_password = 'ATTACKER'\n",
		"auth.api_key = \"ATTACKER\"\n", // a form the line rewrite does not see
	} {
		arc, err := Open(craft(t, map[string]string{ManifestName: manifestFor(nameConfig, cfg), nameConfig: cfg}), "")
		if err != nil {
			t.Fatalf("%q: checksums are consistent, Open must accept it: %v", cfg, err)
		}
		local := newLayout(t)
		fresh := Layout{ConfigPath: filepath.Join(t.TempDir(), "config.toml"), DataDir: t.TempDir()}
		for _, l := range []Layout{local, fresh} {
			if _, err := arc.Plan(l); !errors.Is(err, ErrNotRedacted) {
				t.Errorf("%q: Plan err = %v, want ErrNotRedacted", cfg, err)
			}
		}
		if got := read(t, local.ConfigPath); got != testConfig {
			t.Fatalf("%q: local config changed:\n%s", cfg, got)
		}
		if _, err := os.Stat(fresh.ConfigPath); !os.IsNotExist(err) {
			t.Errorf("%q: config written on the fresh machine: %v", cfg, err)
		}
	}
}

// Ordinary variations the line rewrite does handle must still produce a valid,
// secret-free config.
func TestRedactionHandlesCommonForms(t *testing.T) {
	for _, cfg := range []string{
		"[auth]\r\napi_key = \"S3CRET\"\r\napi_url = \"https://x\"\r\n",
		"[ auth ]  # creds\napi_key=\"S3CRET\"   # mine\n",
		"[auth]\napi_key = 'S3CRET'\n",
		"[auth]\napi_key = \"S3\\\"CRET\"\n",
		"[downloads.vpn]\nenabled = true\n[downloads]\nwebdav_password = \"S3CRET\"\n",
	} {
		out, err := redactConfig([]byte(cfg))
		if err != nil {
			t.Errorf("%q: %v", cfg, err)
			continue
		}
		if strings.Contains(string(out), "S3") {
			t.Errorf("%q: secret survived:\n%s", cfg, out)
		}
	}
}

// TestRedactTasksStripsEveryAgentTaskSecret marshals the REAL agent types, so a
// renamed JSON tag on a secret field fails here instead of leaking.
func TestRedactTasksStripsEveryAgentTaskSecret(t *testing.T) {
	task := agent.Task{
		ID: "t1", InfoHash: "keep-hash", DirectURL: "SECRET-A", NzbPassword: "SECRET-B",
		SourceSet: &agent.SourceSet{Version: 1, Sources: []agent.Source{
			{ID: "s1", Transport: "debrid", Provider: "keep-provider", DirectURL: "SECRET-C", Password: "SECRET-D"},
		}},
	}
	raw, err := json.Marshal([]agent.Task{task})
	if err != nil {
		t.Fatal(err)
	}
	out, err := redactTasks(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "SECRET") {
		t.Fatalf("task secret survived:\n%s", out)
	}
	var back []agent.Task
	if err := json.Unmarshal(out, &back); err != nil || back[0].InfoHash != "keep-hash" || back[0].SourceSet.Sources[0].Provider != "keep-provider" {
		t.Fatalf("queue not preserved: %v %+v", err, back)
	}
}

// The backup reads active-tasks.json from the same place the daemon's store
// writes it, and the queue check accepts what the store writes; this fails if
// either side moves the file or changes its shape.
func TestLayoutReadsTheAgentTaskStore(t *testing.T) {
	root := t.TempDir()
	t.Setenv("UNARR_CONFIG_DIR", filepath.Join(root, "cfg"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "xdg"))
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "local"))
	agent.NewActiveTaskStore().Add(agent.Task{ID: "store-task"})

	files, err := collect(Layout{ConfigPath: filepath.Join(root, "none.toml"), DataDir: config.DataDir()}, false, KindManual)
	if err != nil || len(files) != 1 || !strings.Contains(string(files[0].data), "store-task") {
		t.Fatalf("backup does not see the agent's queue file: %v %s", err, fmt.Sprint(files))
	}
}
