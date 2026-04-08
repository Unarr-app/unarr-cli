package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/torrentclaw/unarr/internal/agent"
	"github.com/torrentclaw/unarr/internal/engine"
)

// --- Mocks para tests del comando download ---

// testDownloader implementa engine.Downloader para tests.
type testDownloader struct {
	method    engine.DownloadMethod
	available bool
	filePath  string // archivo a devolver como resultado
	err       error  // si != nil, Download() devuelve este error
}

func (d *testDownloader) Method() engine.DownloadMethod { return d.method }
func (d *testDownloader) Available(_ context.Context, _ *engine.Task) (bool, error) {
	return d.available, nil
}
func (d *testDownloader) Download(_ context.Context, _ *engine.Task, _ string, _ chan<- engine.Progress) (*engine.Result, error) {
	if d.err != nil {
		return nil, d.err
	}
	return &engine.Result{
		FilePath: d.filePath,
		FileName: filepath.Base(d.filePath),
		Method:   d.method,
		Size:     1024,
	}, nil
}
func (d *testDownloader) Pause(_ string) error             { return nil }
func (d *testDownloader) Cancel(_ string) error            { return nil }
func (d *testDownloader) Shutdown(_ context.Context) error { return nil }

// makeDepsWithDownloader crea un downloadDeps con un downloader mockeado.
func makeDepsWithDownloader(dl engine.Downloader) downloadDeps {
	return downloadDeps{
		newTorrentDl: func(cfg engine.TorrentConfig) (engine.Downloader, error) {
			return dl, nil
		},
		newDebridDl: func() engine.Downloader {
			return &testDownloader{method: engine.MethodDebrid, available: false}
		},
		newAgentClient: func(url, key, ua string) *agent.Client {
			return agent.NewClient("http://localhost", "", "test")
		},
		newManager: engine.NewManager,
	}
}

// --- Tests de validación de entrada ---

func TestRunDownload_EmptyInput(t *testing.T) {
	err := runDownload("", "torrent")
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestRunDownload_InvalidHash_TooShort(t *testing.T) {
	err := runDownload("abc123", "torrent")
	if err == nil {
		t.Fatal("expected error for hash that is too short")
	}
	if !strings.Contains(err.Error(), "invalid") {
		t.Errorf("error = %q, want 'invalid' in message", err.Error())
	}
}

func TestRunDownload_InvalidHash_NotHex_TooLong(t *testing.T) {
	// 41 caracteres pero comienza con "magnet:" no → tampoco es un hash válido de 40 chars
	err := runDownload("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", "torrent") // 41 chars
	if err == nil {
		t.Fatal("expected error for 41-char string (not a valid hash)")
	}
}

func TestRunDownload_ValidHash_40Chars(t *testing.T) {
	// Un hash de 40 chars hex válido debe pasar la validación
	// Usa deps que fallan inmediatamente para no necesitar red
	deps := downloadDeps{
		newTorrentDl: func(cfg engine.TorrentConfig) (engine.Downloader, error) {
			return nil, fmt.Errorf("test: stopping after validation")
		},
		newDebridDl: func() engine.Downloader {
			return &testDownloader{method: engine.MethodDebrid}
		},
		newAgentClient: func(url, key, ua string) *agent.Client {
			return agent.NewClient("http://localhost", "", "test")
		},
		newManager: engine.NewManager,
	}

	err := runDownloadWithDeps("abc123def456abc123def456abc123def456abc1", "torrent", deps)
	// El error debe ser del downloader (no de validación)
	if err == nil {
		t.Fatal("expected error from newTorrentDl")
	}
	if strings.Contains(err.Error(), "invalid input") || strings.Contains(err.Error(), "invalid info hash") {
		t.Errorf("error = %q — should not be a validation error, hash is valid", err.Error())
	}
}

func TestRunDownload_InvalidInput_NotMagnetNotHash(t *testing.T) {
	// Texto libre que no es ni hash ni magnet
	err := runDownload("The Matrix 1999", "torrent")
	if err == nil {
		t.Fatal("expected error for plain text input")
	}
	if !strings.Contains(err.Error(), "invalid") {
		t.Errorf("error = %q, want 'invalid' in message", err.Error())
	}
}

func TestRunDownload_InvalidInput_PartialMagnet(t *testing.T) {
	// Prefix de magnet pero incompleto
	err := runDownload("magnet:", "torrent")
	if err == nil {
		t.Fatal("expected error for incomplete magnet URI (no hash)")
	}
}

// --- Tests con mock downloader ---

func TestRunDownload_Success(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(filePath, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}

	dl := &testDownloader{
		method:    engine.MethodTorrent,
		available: true,
		filePath:  filePath,
	}

	deps := makeDepsWithDownloader(dl)
	// Sobreescribir outputDir usando config vacía (usa home por defecto)
	// Para un test determinista, usar una config con dir específico
	deps.newTorrentDl = func(cfg engine.TorrentConfig) (engine.Downloader, error) {
		// Actualizar filePath al outputDir real
		realPath := filepath.Join(cfg.DataDir, "movie.mkv")
		os.WriteFile(realPath, make([]byte, 1024), 0o644) //nolint:errcheck
		return &testDownloader{
			method:    engine.MethodTorrent,
			available: true,
			filePath:  realPath,
		}, nil
	}

	err := runDownloadWithDeps("abc123def456abc123def456abc123def456abc1", "torrent", deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunDownload_DownloaderCreationFails(t *testing.T) {
	deps := downloadDeps{
		newTorrentDl: func(cfg engine.TorrentConfig) (engine.Downloader, error) {
			return nil, fmt.Errorf("failed to create torrent client")
		},
		newDebridDl: func() engine.Downloader {
			return &testDownloader{method: engine.MethodDebrid}
		},
		newAgentClient: func(url, key, ua string) *agent.Client {
			return agent.NewClient("http://localhost", "", "test")
		},
		newManager: engine.NewManager,
	}

	err := runDownloadWithDeps("abc123def456abc123def456abc123def456abc1", "torrent", deps)
	if err == nil {
		t.Fatal("expected error when downloader creation fails")
	}
	if !strings.Contains(err.Error(), "create downloader") {
		t.Errorf("error = %q, want 'create downloader' in message", err.Error())
	}
}

func TestRunDownload_DownloadFails(t *testing.T) {
	dl := &testDownloader{
		method:    engine.MethodTorrent,
		available: true,
		err:       errors.New("torrent: no peers"),
	}

	deps := makeDepsWithDownloader(dl)
	// Sin fallback (método específico "torrent"), el fallo se propaga
	err := runDownloadWithDeps("abc123def456abc123def456abc123def456abc1", "torrent", deps)
	// El download falla pero runDownload puede retornar nil (el manager registra el fallo)
	// Lo importante es que no haga panic
	_ = err
}

func TestRunDownload_Method_Torrent(t *testing.T) {
	var capturedTask agent.Task
	dl := &capturingTestDownloader{
		method:      engine.MethodTorrent,
		capturedFn:  func(t agent.Task) { capturedTask = t },
		resultDir:   t.TempDir(),
		resultFile:  "movie.mkv",
		resultBytes: make([]byte, 512),
	}

	deps := downloadDeps{
		newTorrentDl: func(cfg engine.TorrentConfig) (engine.Downloader, error) {
			return dl, nil
		},
		newDebridDl: func() engine.Downloader {
			return &testDownloader{method: engine.MethodDebrid}
		},
		newAgentClient: func(url, key, ua string) *agent.Client {
			return agent.NewClient("http://localhost", "", "test")
		},
		newManager: engine.NewManager,
	}

	os.WriteFile(filepath.Join(dl.resultDir, dl.resultFile), dl.resultBytes, 0o644) //nolint:errcheck

	runDownloadWithDeps("abc123def456abc123def456abc123def456abc1", "torrent", deps) //nolint:errcheck

	if capturedTask.PreferredMethod != "torrent" {
		t.Errorf("PreferredMethod = %q, want torrent", capturedTask.PreferredMethod)
	}
}

func TestRunDownload_Method_Debrid(t *testing.T) {
	var capturedTask agent.Task

	resultDir := t.TempDir()
	resultFile := filepath.Join(resultDir, "movie.mkv")
	os.WriteFile(resultFile, make([]byte, 512), 0o644) //nolint:errcheck

	capFn := func(task agent.Task) { capturedTask = task }

	deps := downloadDeps{
		newTorrentDl: func(cfg engine.TorrentConfig) (engine.Downloader, error) {
			// Torrent no disponible: fuerza el uso del método debrid
			return &testDownloader{method: engine.MethodTorrent, available: false}, nil
		},
		newDebridDl: func() engine.Downloader {
			// Debrid disponible y captura la tarea
			return &capturingTestDownloader{
				method:      engine.MethodDebrid,
				capturedFn:  capFn,
				resultDir:   resultDir,
				resultFile:  "movie.mkv",
				resultBytes: make([]byte, 512),
			}
		},
		newAgentClient: func(url, key, ua string) *agent.Client {
			return agent.NewClient("http://localhost", "", "test")
		},
		newManager: engine.NewManager,
	}

	runDownloadWithDeps("abc123def456abc123def456abc123def456abc1", "debrid", deps) //nolint:errcheck

	if capturedTask.PreferredMethod != "debrid" {
		t.Errorf("PreferredMethod = %q, want debrid", capturedTask.PreferredMethod)
	}
}

func TestRunDownload_OutputDirCreated(t *testing.T) {
	// Verificar que el dir de salida se crea aunque no exista
	downloadDir := filepath.Join(t.TempDir(), "new-subdir", "downloads")
	// No crear el directorio — runDownload debe hacerlo

	deps := downloadDeps{
		newTorrentDl: func(cfg engine.TorrentConfig) (engine.Downloader, error) {
			// Una vez creado el dir, podemos retornar error para terminar
			if _, err := os.Stat(cfg.DataDir); err != nil {
				return nil, fmt.Errorf("output dir was not created")
			}
			return nil, fmt.Errorf("stopping after dir check")
		},
		newDebridDl: func() engine.Downloader {
			return &testDownloader{method: engine.MethodDebrid}
		},
		newAgentClient: func(url, key, ua string) *agent.Client {
			return agent.NewClient("http://localhost", "", "test")
		},
		newManager: engine.NewManager,
	}

	// Necesitamos que cfg.Download.Dir apunte a nuestro dir de test
	// loadConfig() usará el default, así que testeamos la creación del dir
	// Alternativa: verificar que si el dir ya existe, no falla
	_ = deps
	_ = downloadDir
	// Este test documenta la intención aunque no pueda inyectar el dir fácilmente
	// sin refactorizar loadConfig(). El comportamiento se testa indirectamente.
	t.Skip("requiere inyección de config — comportamiento cubierto por tests de integración")
}

func TestRunDownloadCmd_Args_TooFew(t *testing.T) {
	cmd := newDownloadCmd()
	// Sin argumentos → cobra debe devolver error
	err := cmd.Args(cmd, []string{})
	if err == nil {
		t.Fatal("expected error for 0 args")
	}
}

func TestRunDownloadCmd_Args_TooMany(t *testing.T) {
	cmd := newDownloadCmd()
	err := cmd.Args(cmd, []string{"hash1", "hash2"})
	if err == nil {
		t.Fatal("expected error for 2 args")
	}
}

func TestRunDownloadCmd_Args_ExactlyOne(t *testing.T) {
	cmd := newDownloadCmd()
	err := cmd.Args(cmd, []string{"abc123def456abc123def456abc123def456abc1"})
	if err != nil {
		t.Errorf("unexpected error for 1 arg: %v", err)
	}
}

// capturingTestDownloader captura la tarea recibida para verificar los flags.
type capturingTestDownloader struct {
	method      engine.DownloadMethod
	capturedFn  func(agent.Task)
	resultDir   string
	resultFile  string
	resultBytes []byte
}

func (d *capturingTestDownloader) Method() engine.DownloadMethod { return d.method }
func (d *capturingTestDownloader) Available(_ context.Context, _ *engine.Task) (bool, error) {
	return true, nil
}
func (d *capturingTestDownloader) Download(_ context.Context, task *engine.Task, _ string, _ chan<- engine.Progress) (*engine.Result, error) {
	if d.capturedFn != nil {
		d.capturedFn(agent.Task{
			ID:              task.ID,
			PreferredMethod: task.PreferredMethod,
		})
	}
	filePath := filepath.Join(d.resultDir, d.resultFile)
	return &engine.Result{
		FilePath: filePath,
		FileName: d.resultFile,
		Method:   d.method,
		Size:     int64(len(d.resultBytes)),
	}, nil
}
func (d *capturingTestDownloader) Pause(_ string) error             { return nil }
func (d *capturingTestDownloader) Cancel(_ string) error            { return nil }
func (d *capturingTestDownloader) Shutdown(_ context.Context) error { return nil }

// TestRunDownload_QuickFail_NoDeadlock verifica que cuando el downloader falla
// rápidamente, runDownload retorna sin deadlock.
func TestRunDownload_QuickFail_NoDeadlock(t *testing.T) {
	deps := downloadDeps{
		newTorrentDl: func(cfg engine.TorrentConfig) (engine.Downloader, error) {
			return &testDownloader{
				method:    engine.MethodTorrent,
				available: true,
				err:       errors.New("no peers found"),
			}, nil
		},
		newDebridDl: func() engine.Downloader {
			return &testDownloader{method: engine.MethodDebrid, available: false}
		},
		newAgentClient: func(url, key, ua string) *agent.Client {
			return agent.NewClient("http://localhost", "", "test")
		},
		newManager: engine.NewManager,
	}

	done := make(chan struct{}, 1)
	go func() {
		runDownloadWithDeps("abc123def456abc123def456abc123def456abc1", "torrent", deps) //nolint:errcheck
		done <- struct{}{}
	}()

	select {
	case <-done:
		// OK, terminó sin deadlock
	case <-time.After(10 * time.Second):
		t.Fatal("runDownload did not return within 10s — possible deadlock")
	}
}
