package support

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/Unarr-app/unarr-cli/internal/agent"
)

// maxTitleRunes bounds a published task title. A title is the user's media
// library leaking into a public issue; the first characters are enough to
// correlate a task with a log line, and the rest is nobody's business.
const maxTitleRunes = 24

// publishedState is the allowlist projection of agent.DaemonState.
//
// Same rule as publishedConfig, and for the same reason: the state file grows
// fields over time (StreamSecret and FunnelURL both landed after it shipped),
// and a struct that enumerates what may be published cannot be surprised by
// the next one.
type publishedState struct {
	Status          string    `json:"status"`
	Version         string    `json:"version"`
	PID             int       `json:"pid"`
	StartedAt       time.Time `json:"startedAt"`
	LastHeartbeat   time.Time `json:"lastHeartbeat"`
	UptimeSeconds   int64     `json:"uptimeSeconds"`
	HeartbeatAgeSec int64     `json:"heartbeatAgeSeconds"`
	ActiveTasks     int       `json:"activeTasks"`
	CompletedCount  int       `json:"completedCount"`
	FailedCount     int       `json:"failedCount"`
	TotalDownloaded int64     `json:"totalDownloaded"`

	MethodStats map[string]int `json:"methodStats,omitempty"`

	// LogFile is published as a basename only: the directory is the user's home.
	LogFileName string `json:"logFileName,omitempty"`

	// The agent ID identifies the install but is withheld here for the same
	// reason it is withheld from the config — support can look it up from the
	// doctor report the user ran.
	AgentIDConfigured bool `json:"agentIdConfigured"`

	VPNActive   bool   `json:"vpnActive"`
	VPNRequired bool   `json:"vpnRequired"`
	VPNBlocking bool   `json:"vpnBlocking"`
	VPNMode     string `json:"vpnMode,omitempty"`
	// FunnelActive, not FunnelURL: the quick-tunnel hostname is a bearer
	// capability — whoever has it can reach this agent from the internet.
	FunnelActive bool `json:"funnelActive"`
}

// daemonStateJSON publishes the running daemon's state file.
func daemonStateJSON() ([]byte, error) {
	st, err := agent.LoadState()
	if err != nil {
		return nil, fmt.Errorf("absent: %w", err)
	}
	if st == nil {
		return nil, errors.New("absent: no daemon state on disk")
	}
	return marshalIndent(projectState(st, time.Now()))
}

// projectState is the pure projection, split out so a test can pin the derived
// ages without a running daemon.
func projectState(st *agent.DaemonState, now time.Time) publishedState {
	out := publishedState{
		Status:            st.Status,
		Version:           st.Version,
		PID:               st.PID,
		StartedAt:         st.StartedAt,
		LastHeartbeat:     st.LastHeartbeat,
		ActiveTasks:       st.ActiveTasks,
		CompletedCount:    st.CompletedCount,
		FailedCount:       st.FailedCount,
		TotalDownloaded:   st.TotalDownloaded,
		MethodStats:       st.MethodStats,
		AgentIDConfigured: st.AgentID != "",
		VPNActive:         st.VPNActive,
		VPNRequired:       st.VPNRequired,
		VPNBlocking:       st.VPNBlocking,
		VPNMode:           pick(st.VPNMode, "managed", "self-hosted"),
		FunnelActive:      st.FunnelURL != "",
	}
	if st.LogFile != "" {
		out.LogFileName = filepath.Base(st.LogFile)
	}
	// Derived ages, because "last heartbeat 41 minutes ago" is the finding and
	// a raw timestamp makes the reader do date arithmetic in an issue thread.
	if !st.StartedAt.IsZero() {
		out.UptimeSeconds = int64(now.Sub(st.StartedAt).Seconds())
	}
	if !st.LastHeartbeat.IsZero() {
		out.HeartbeatAgeSec = int64(now.Sub(st.LastHeartbeat).Seconds())
	}
	return out
}

// publishedTask is the allowlist projection of agent.Task.
//
// The dispatch payload is full of things that must not travel: DirectURL is a
// signed debrid link (a download credential with an expiry), NzbPassword is a
// password by name, and FilePath/ReplacePath are absolute paths into the user's
// library. What support actually needs from a stuck task is which method it
// chose and roughly what it was.
type publishedTask struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	PreferredMethod string `json:"preferredMethod,omitempty"`
	Mode            string `json:"mode,omitempty"`
	ContentType     string `json:"contentType,omitempty"`
	HasSeason       bool   `json:"hasSeason,omitempty"`
	ViaDirectURL    bool   `json:"viaDirectUrl,omitempty"`
	ViaNZB          bool   `json:"viaNzb,omitempty"`
	Encrypted       bool   `json:"encrypted,omitempty"`
	IsUpgrade       bool   `json:"isUpgrade,omitempty"`
}

// activeTasksJSON publishes the downloads the daemon would resume on restart.
// A task stuck in this file across restarts is a real and recurring bug shape,
// and it is invisible from anywhere else.
func activeTasksJSON() ([]byte, error) {
	tasks := agent.NewActiveTaskStore().Load()
	if len(tasks) == 0 {
		return nil, errors.New("absent: no active tasks recorded (nothing in flight, or the daemon never ran)")
	}
	out := make([]publishedTask, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, projectTask(t))
	}
	return marshalIndent(out)
}

func projectTask(t agent.Task) publishedTask {
	return publishedTask{
		ID:              t.ID,
		Title:           truncateRunes(t.Title, maxTitleRunes),
		PreferredMethod: pick(t.PreferredMethod, methodVocab...),
		Mode:            pick(t.Mode, "download", "stream", "seed_file"),
		ContentType:     pick(t.ContentType, "movie", "show"),
		HasSeason:       t.Season != nil,
		ViaDirectURL:    t.DirectURL != "",
		ViaNZB:          t.NzbID != "",
		Encrypted:       t.NzbPassword != "",
		IsUpgrade:       t.ReplacePath != "",
	}
}

// truncateRunes cuts on rune boundaries so a multi-byte title does not come out
// as mojibake — the bundle is read by humans, and half a UTF-8 sequence reads
// as corruption in the tool rather than as a deliberate trim.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func marshalIndent(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
