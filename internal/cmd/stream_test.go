package cmd

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/torrentclaw/unarr/internal/engine"
)

// --- Tests de validación de entrada para runStream ---

func TestRunStream_EmptyInput(t *testing.T) {
	err := runStream("", 0, true, "")
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestRunStream_InvalidInput_NotHashNotMagnet(t *testing.T) {
	err := runStream("The Matrix 1999", 0, true, "")
	if err == nil {
		t.Fatal("expected error for plain text input")
	}
	if !strings.Contains(err.Error(), "invalid") {
		t.Errorf("error = %q, want 'invalid' in message", err.Error())
	}
}

func TestRunStream_InvalidInput_TooShort(t *testing.T) {
	err := runStream("abc123", 0, true, "")
	if err == nil {
		t.Fatal("expected error for hash too short")
	}
}

func TestRunStream_ValidHash_PassesValidation(t *testing.T) {
	// Un hash válido debe pasar la validación y llegar a newStreamEngine.
	// Inyectamos un engine que falla inmediatamente para no necesitar red.
	deps := streamDeps{
		newStreamEngine: func(cfg engine.StreamConfig) (*engine.StreamEngine, error) {
			return nil, fmt.Errorf("test: stopping after validation")
		},
		newStreamServer: engine.NewStreamServer,
		openPlayer: func(url, override string) (string, *exec.Cmd, error) {
			return "", nil, nil
		},
	}

	err := runStreamWithDeps("abc123def456abc123def456abc123def456abc1", 0, true, "", deps)
	if err == nil {
		t.Fatal("expected error from newStreamEngine mock")
	}
	// El error debe venir del engine, no de validación
	if strings.Contains(err.Error(), "invalid input") {
		t.Errorf("error = %q — should not be a validation error, hash is valid", err.Error())
	}
	if !strings.Contains(err.Error(), "create stream engine") {
		t.Errorf("error = %q — expected 'create stream engine' from engine creation failure", err.Error())
	}
}

func TestRunStream_MagnetURI_PassesValidation(t *testing.T) {
	deps := streamDeps{
		newStreamEngine: func(cfg engine.StreamConfig) (*engine.StreamEngine, error) {
			return nil, fmt.Errorf("test: stopping after validation")
		},
		newStreamServer: engine.NewStreamServer,
		openPlayer: func(url, override string) (string, *exec.Cmd, error) {
			return "", nil, nil
		},
	}

	magnet := "magnet:?xt=urn:btih:abc123def456abc123def456abc123def456abc1&dn=Test"
	err := runStreamWithDeps(magnet, 0, true, "", deps)
	if err == nil {
		t.Fatal("expected error from newStreamEngine mock")
	}
	if strings.Contains(err.Error(), "invalid input") {
		t.Errorf("magnet URI should be valid, got validation error: %v", err)
	}
}

func TestRunStream_EngineCreationFails(t *testing.T) {
	deps := streamDeps{
		newStreamEngine: func(cfg engine.StreamConfig) (*engine.StreamEngine, error) {
			return nil, fmt.Errorf("failed to create torrent client")
		},
		newStreamServer: engine.NewStreamServer,
		openPlayer: func(url, override string) (string, *exec.Cmd, error) {
			return "", nil, nil
		},
	}

	err := runStreamWithDeps("abc123def456abc123def456abc123def456abc1", 0, true, "", deps)
	if err == nil {
		t.Fatal("expected error when engine creation fails")
	}
	if !strings.Contains(err.Error(), "create stream engine") {
		t.Errorf("error = %q, want 'create stream engine' in message", err.Error())
	}
}

func TestRunStreamCmd_Args_TooFew(t *testing.T) {
	cmd := newStreamCmd()
	err := cmd.Args(cmd, []string{})
	if err == nil {
		t.Fatal("expected error for 0 args")
	}
}

func TestRunStreamCmd_Args_TooMany(t *testing.T) {
	cmd := newStreamCmd()
	err := cmd.Args(cmd, []string{"hash1", "hash2"})
	if err == nil {
		t.Fatal("expected error for 2 args")
	}
}

func TestRunStreamCmd_Args_ExactlyOne(t *testing.T) {
	cmd := newStreamCmd()
	err := cmd.Args(cmd, []string{"abc123def456abc123def456abc123def456abc1"})
	if err != nil {
		t.Errorf("unexpected error for 1 arg: %v", err)
	}
}

func TestRunStream_PartialMagnet_Prefix(t *testing.T) {
	// "magnet:" sin hash es válido para el parser (tiene el prefijo magnet:)
	// pero no tiene infoHash — debe pasar la validación de input
	deps := streamDeps{
		newStreamEngine: func(cfg engine.StreamConfig) (*engine.StreamEngine, error) {
			return nil, fmt.Errorf("test stop")
		},
		newStreamServer: engine.NewStreamServer,
		openPlayer:      func(url, override string) (string, *exec.Cmd, error) { return "", nil, nil },
	}
	// "magnet:" sin btih se trata como magnet (HasPrefix("magnet:") == true)
	// por lo que pasa la validación de input
	err := runStreamWithDeps("magnet:", 0, true, "", deps)
	// Debe llegar al engine (validación OK) o fallar con error de engine
	_ = err // no verificamos el contenido exacto, solo que no haya panic
}

func TestRunStream_NoOpen_DoesNotCallOpenPlayer(t *testing.T) {
	playerCalled := false
	deps := streamDeps{
		newStreamEngine: func(cfg engine.StreamConfig) (*engine.StreamEngine, error) {
			return nil, fmt.Errorf("test: stopping early")
		},
		newStreamServer: engine.NewStreamServer,
		openPlayer: func(url, override string) (string, *exec.Cmd, error) {
			playerCalled = true
			return "mpv", nil, nil
		},
	}

	// noOpen=true → openPlayer no debe llamarse
	runStreamWithDeps("abc123def456abc123def456abc123def456abc1", 0, true, "", deps) //nolint:errcheck

	if playerCalled {
		t.Error("openPlayer should NOT be called when noOpen=true")
	}
}
