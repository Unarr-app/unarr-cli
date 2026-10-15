package backup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/BurntSushi/toml"

	"github.com/Unarr-app/unarr-cli/internal/agent"
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

// checkTasks refuses a queue the daemon would silently drop on load: its store
// unmarshals the whole file into []agent.Task and starts empty when that fails,
// so one field of the wrong type, or a queuedAt that is not a time, loses every
// task.
func checkTasks(data []byte) error {
	if err := decodeTasks(data); err != nil {
		return fmt.Errorf("%w: %s is not a list of tasks the daemon can load: %v", ErrBadContent, nameTasks, err)
	}
	return nil
}

// decodeTasks fails exactly when json.Unmarshal into []agent.Task — the store's
// own load — would, but decodes one task at a time, so the queue is never held
// in memory a second time.
func decodeTasks(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return emptyIsUnexpected(err)
	}
	switch tok {
	case nil: // null: Unmarshal leaves the list empty, without an error
	case json.Delim('['):
		for dec.More() {
			var t agent.Task // fresh each time: a null element decodes to nothing
			if err := dec.Decode(&t); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // the closing ]
			return emptyIsUnexpected(err)
		}
	default:
		return fmt.Errorf("found %v where a list should start", tok)
	}
	// Unmarshal refuses anything after the one value: more JSON or not.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("more data after the list")
	}
	return nil
}

// emptyIsUnexpected words input that ends too soon the way Unmarshal does.
func emptyIsUnexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
