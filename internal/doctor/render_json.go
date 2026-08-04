package doctor

import (
	"encoding/json"
	"io"
)

// RenderJSON writes the report as a single JSON object.
//
// Unlike the text renderer this one is not streaming — a JSON document can only
// be emitted once the run is complete — which is why Run's callback is optional.
// The shape is flat on purpose so a Docker HEALTHCHECK can decide with one
// expression (`jq -e '.status != "fail"'` or `.failed == 0`) and so
// support-bundle can embed the object verbatim.
func RenderJSON(w io.Writer, rep Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}
