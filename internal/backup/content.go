package backup

import (
	"encoding/json"
	"fmt"

	"github.com/BurntSushi/toml"

	"github.com/Unarr-app/unarr-cli/internal/config"
)

// decodeConfig decodes config.toml the way config.Load does — the same decoder
// into the same struct — so "it decodes here" means the CLI and the daemon can
// load it.
func decodeConfig(data []byte) (config.Config, error) {
	var c config.Config
	_, err := toml.Decode(string(data), &c)
	return c, err
}

// checkConfig refuses a config.toml unarr could not load: every command would
// warn that it does not parse and run on the defaults — signed out, with every
// setting gone.
func checkConfig(data []byte) error {
	if _, err := decodeConfig(data); err != nil {
		return fmt.Errorf("%w: %s is not valid TOML: %v", ErrBadContent, nameConfig, err)
	}
	return nil
}

// checkTasks refuses a queue the daemon would silently drop on load: it must be
// a JSON list of task objects, the shape the daemon's store and redactTasks
// read. Decoding into empty structs checks that shape without holding the
// tasks in memory a second time.
func checkTasks(data []byte) error {
	var tasks []struct{}
	if err := json.Unmarshal(data, &tasks); err != nil {
		return fmt.Errorf("%w: %s is not a JSON list of tasks: %v", ErrBadContent, nameTasks, err)
	}
	return nil
}
