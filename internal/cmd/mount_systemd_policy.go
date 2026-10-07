package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"

	"github.com/Unarr-app/unarr-cli/internal/config"
	"github.com/Unarr-app/unarr-cli/internal/service"
)

const systemdMountPolicyName = "90-unarr-remote-mount.conf"
const systemdMountPolicyBody = "# Managed by unarr for remote mount cleanup.\n[Service]\nKillMode=mixed\n"

type systemdMountUnit struct {
	loadState string
	fragment  string
	dropIns   string
	killMode  string
}

// Service control must read the service's persisted HOME configuration, not
// appCfg, --config, UNARR_CONFIG_DIR or environment-overridden mount settings.
func guardDefaultSystemdMountPolicy() (bool, error) {
	if runtime.GOOS != "linux" {
		return false, nil
	}
	data, err := resolveServiceData()
	if err != nil {
		return false, err
	}
	cfg, err := config.Load(filepath.Join(data.Home, ".config", "unarr", "config.toml"))
	if err != nil {
		return false, err
	}
	return prepareSystemdMountPolicy(data, cfg)
}

func guardMountSystemdPolicy(cfg config.Config) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	data, err := resolveServiceData()
	if err != nil {
		return err
	}
	_, err = prepareSystemdMountPolicy(data, cfg)
	return err
}

func systemdMountPolicyPath(home string) string {
	return filepath.Join(service.SystemdUnitPathIn(home)+".d", systemdMountPolicyName)
}

func recognizedSystemdMountPolicy(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	body, err := os.ReadFile(path)
	return err == nil && string(body) == systemdMountPolicyBody
}

// No mount intention and no recognized owned override leaves ordinary/custom
// service control untouched. Saved-false mount configurations retain Directory.
func prepareSystemdMountPolicy(data serviceData, cfg config.Config) (bool, error) {
	policyPath := systemdMountPolicyPath(data.Home)
	if !cfg.Mount.Enabled && cfg.Mount.Directory == "" && !recognizedSystemdMountPolicy(policyPath) {
		return false, nil
	}
	unit, err := readSystemdMountUnit()
	if err != nil {
		return false, err
	}
	base := service.SystemdUnitPathIn(data.Home)
	if unit.loadState == "not-found" && unit.fragment == "" && unit.dropIns == "" {
		if _, err := os.Lstat(base); errors.Is(err, os.ErrNotExist) && !recognizedSystemdMountPolicy(policyPath) {
			// A missing base can hide orphaned overrides that a fresh install
			// would load. Preserve and reject those before saving activation.
			_, err := validateSystemdMountDropIns(data.Home, "")
			return false, err // A fresh install will write the mixed template.
		}
	}
	legacy, owned, err := validateSystemdMountUnit(data, unit)
	if err != nil {
		return false, err
	}
	if unit.killMode == "mixed" {
		return true, nil
	}
	if unit.killMode != "control-group" {
		return false, fmt.Errorf("remote mount service has unsupported KillMode %q; preserve custom policy and review the installed unit", unit.killMode)
	}
	if err := applySystemdMountPolicy(data, legacy && !owned, writeServiceFile); err != nil {
		return false, err
	}
	return true, nil
}

func readSystemdMountUnit() (systemdMountUnit, error) {
	out, err := svcOutput("systemctl", "--user", "show", "unarr.service", "-p", "LoadState", "-p", "FragmentPath", "-p", "DropInPaths", "-p", "KillMode")
	if err != nil {
		return systemdMountUnit{}, fmt.Errorf("inspect remote mount service: %w: %s", err, out)
	}
	var unit systemdMountUnit
	seen := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || seen[key] {
			return unit, errors.New("remote mount service returned incomplete or ambiguous properties")
		}
		seen[key] = true
		switch key {
		case "LoadState":
			unit.loadState = value
		case "FragmentPath":
			unit.fragment = value
		case "DropInPaths":
			unit.dropIns = value
		case "KillMode":
			unit.killMode = value
		default:
			return unit, errors.New("remote mount service returned unexpected properties")
		}
	}
	if len(seen) != 4 {
		return unit, errors.New("remote mount service returned incomplete properties")
	}
	return unit, nil
}

func validateSystemdMountUnit(data serviceData, unit systemdMountUnit) (legacy, owned bool, err error) {
	base := service.SystemdUnitPathIn(data.Home)
	if unit.loadState != "loaded" || unit.fragment != base {
		return false, false, errors.New("remote mount requires the canonical unarr user unit; the effective service is different or not loaded")
	}
	info, err := os.Lstat(base)
	if err != nil || !info.Mode().IsRegular() {
		return false, false, errors.New("remote mount requires a regular unarr unit, without symlinks")
	}
	real, err := filepath.EvalSymlinks(base)
	if err != nil || real != base {
		return false, false, errors.New("remote mount unit path must be canonical, without symlinks")
	}
	body, err := os.ReadFile(base)
	if err != nil {
		return false, false, err
	}
	current, err := renderSystemdMountUnit(systemdTemplate, data)
	if err != nil {
		return false, false, err
	}
	previous, err := renderSystemdMountUnit(strings.Replace(systemdTemplate, "KillMode=mixed\n", "", 1), data)
	if err != nil {
		return false, false, err
	}
	legacy = bytes.Equal(body, previous)
	if !legacy && !bytes.Equal(body, current) {
		return false, false, errors.New("remote mount will not modify a custom unarr unit; preserve it and use mount serve or review the default daemon installation")
	}
	owned, err = validateSystemdMountDropIns(data.Home, unit.dropIns)
	return legacy, owned, err
}

func renderSystemdMountUnit(body string, data serviceData) ([]byte, error) {
	tmpl, err := template.New("mount-service").Parse(body)
	if err != nil {
		return nil, err
	}
	var rendered bytes.Buffer
	err = tmpl.Execute(&rendered, data)
	return rendered.Bytes(), err
}

func validateSystemdMountDropIns(home, effective string) (bool, error) {
	path := systemdMountPolicyPath(home)
	if effective != "" && effective != path {
		return false, errors.New("remote mount will not override external systemd drop-ins")
	}
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) && effective == "" {
		return false, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("remote mount drop-in directory must be a regular directory, without symlinks")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	owned := false
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".conf") {
			continue
		}
		if entry.Name() != systemdMountPolicyName || !recognizedSystemdMountPolicy(path) {
			return false, errors.New("remote mount will not modify foreign or edited systemd drop-ins")
		}
		owned = true
	}
	if effective != "" && !owned {
		return false, errors.New("effective remote mount drop-in is missing on disk")
	}
	return owned, nil
}

func applySystemdMountPolicy(data serviceData, create bool, write func(string, string, serviceData) error) error {
	path := systemdMountPolicyPath(data.Home)
	if create {
		if err := write(path, systemdMountPolicyBody, serviceData{}); err != nil {
			return fmt.Errorf("prepare remote mount service policy: %w", err)
		}
	}
	if err := reloadSystemdMountPolicy(); err != nil {
		return errors.Join(err, rollbackSystemdMountPolicy(path, create))
	}
	unit, err := readSystemdMountUnit()
	if err == nil {
		_, _, err = validateSystemdMountUnit(data, unit)
	}
	if err == nil && unit.killMode != "mixed" {
		err = errors.New("remote mount service did not adopt KillMode=mixed")
	}
	if err != nil {
		return errors.Join(err, rollbackSystemdMountPolicy(path, create))
	}
	return nil
}

func reloadSystemdMountPolicy() error {
	out, err := svcOutput("systemctl", "--user", "daemon-reload")
	if err != nil {
		return fmt.Errorf("reload remote mount service policy: %w: %s", err, out)
	}
	return nil
}

func rollbackSystemdMountPolicy(path string, created bool) error {
	if !created {
		return nil // Existing files were never rewritten.
	}
	if !recognizedSystemdMountPolicy(path) {
		return errors.New("remote mount policy rollback refused: owned file changed")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("rollback remote mount policy: %w", err)
	}
	return reloadSystemdMountPolicy()
}

func removeOwnedSystemdMountPolicy(home string) error {
	path := systemdMountPolicyPath(home)
	if !recognizedSystemdMountPolicy(path) {
		return nil
	}
	return os.Remove(path)
}
