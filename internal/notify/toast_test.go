package notify

import (
	"strings"
	"testing"
)

func TestWindowsToastScriptCarriesTheBody(t *testing.T) {
	s := windowsToastScript("Title", "Body 'quoted' C:\\unarr")
	// $text[1] is $null in PowerShell (a WinRT XmlNodeList is not indexable),
	// which dropped every toast body on Windows. Only .Item(n) reaches it.
	if strings.Contains(s, "$text[") {
		t.Errorf("script indexes the node list with [n]; use .Item(n):\n%s", s)
	}
	if !strings.Contains(s, "$text.Item(1).AppendChild($xml.CreateTextNode('Body ''quoted'' C:\\unarr'))") {
		t.Errorf("body not appended to the second text node (escaped):\n%s", s)
	}
}
