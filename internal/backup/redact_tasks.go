package backup

import "encoding/json"

// JSON names of the secrets in agent.Task (directUrl, nzbPassword) and in each
// agent.Source of its sourceSet (directUrl, password). Kept as raw JSON names so
// fields this version does not know survive the round trip;
// TestRedactTasksStripsEveryAgentTaskSecret marshals real agent types, so a
// renamed tag fails the build instead of leaking.
var (
	taskSecretFields   = []string{"directUrl", "nzbPassword"}
	sourceSecretFields = []string{"directUrl", "password"}
)

// redactTasks strips resolved source URLs (debrid tokens, IPTV credentials) and
// NZB passwords from active-tasks.json. The queue itself survives: a restored
// torrent task resumes from its infoHash; a debrid one keeps its provider and
// file identity but needs the server to issue a fresh link.
func redactTasks(data []byte) ([]byte, error) {
	var tasks []map[string]json.RawMessage
	if err := json.Unmarshal(data, &tasks); err != nil {
		return nil, err
	}
	for _, t := range tasks {
		for _, f := range taskSecretFields {
			delete(t, f)
		}
		if raw, ok := t["sourceSet"]; ok {
			t["sourceSet"] = redactSourceSet(raw)
		}
	}
	return json.MarshalIndent(tasks, "", "  ")
}

func redactSourceSet(raw json.RawMessage) json.RawMessage {
	var set map[string]json.RawMessage
	if json.Unmarshal(raw, &set) != nil {
		return json.RawMessage("null")
	}
	var sources []map[string]json.RawMessage
	if json.Unmarshal(set["sources"], &sources) != nil {
		delete(set, "sources")
	} else {
		for _, s := range sources {
			for _, f := range sourceSecretFields {
				delete(s, f)
			}
		}
		set["sources"], _ = json.Marshal(sources)
	}
	out, err := json.Marshal(set)
	if err != nil {
		return json.RawMessage("null")
	}
	return out
}
