package mountsetup

import (
	"regexp"
	"strings"

	"golang.org/x/sys/windows/registry"
)

var productCode = regexp.MustCompile(`^\{[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}\}$`)

// WinFsp's official 2.x installer cannot upgrade a registered 1.x product in
// place. Use only MSI product codes from HKLM, never execute UninstallString.
func legacyWinFspProduct() string {
	for _, view := range []uint32{registry.WOW64_32KEY, registry.WOW64_64KEY} {
		if code := legacyWinFspInView(view); code != "" {
			return code
		}
	}
	return ""
}

func legacyWinFspInView(view uint32) string {
	root, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`, registry.READ|view)
	if err != nil {
		return ""
	}
	defer root.Close()
	names, err := root.ReadSubKeyNames(-1)
	if err != nil {
		return ""
	}
	for _, code := range names {
		key, err := registry.OpenKey(root, code, registry.QUERY_VALUE|view)
		if err != nil {
			continue
		}
		name, _, _ := key.GetStringValue("DisplayName")
		version, _, _ := key.GetStringValue("DisplayVersion")
		_ = key.Close()
		if isLegacyWinFsp(name, version, code) {
			return code
		}
	}
	return ""
}

func isLegacyWinFsp(name, version, code string) bool {
	return (name == "WinFsp" || strings.HasPrefix(name, "WinFsp ")) && strings.HasPrefix(version, "1.") && productCode.MatchString(code)
}
