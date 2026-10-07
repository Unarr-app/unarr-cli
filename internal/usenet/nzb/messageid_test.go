package nzb

import (
	"strings"
	"testing"
)

func TestBoundaryNZBMessageID(t *testing.T) {
	for _, id := range []string{"x@test&gt;&#13;&#10;DATE&#13;&#10;BODY &lt;y@test", "x&#13;y@test", "x&#10;y@test", "&lt;x@test", "x@test&gt;", "x y@test"} {
		xml := `<nzb><file subject="v.mkv"><segments><segment bytes="1" number="1">` + id + `</segment></segments></file></nzb>`
		if _, err := Parse(strings.NewReader(xml)); err == nil {
			t.Errorf("accepted malformed ID %q", id)
		}
	}
}
