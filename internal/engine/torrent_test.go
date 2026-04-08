package engine

import (
	"context"
	"testing"
	"time"
)

// TestNewTorrentDownloader_ValidConfig verifica que se puede crear un downloader
// con una configuración válida sin errores.
func TestNewTorrentDownloader_ValidConfig(t *testing.T) {
	dir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("NewTorrentDownloader failed: %v", err)
	}
	defer dl.Shutdown(context.Background())
}

// TestTorrentDownloader_Method verifica que Method() devuelve "torrent".
func TestTorrentDownloader_Method(t *testing.T) {
	dir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("NewTorrentDownloader: %v", err)
	}
	defer dl.Shutdown(context.Background())

	if dl.Method() != MethodTorrent {
		t.Errorf("Method() = %q, want %q", dl.Method(), MethodTorrent)
	}
}

// TestTorrentDownloader_Available_WithInfoHash verifica que Available() devuelve
// true cuando la tarea tiene un infoHash.
func TestTorrentDownloader_Available_WithInfoHash(t *testing.T) {
	dir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("NewTorrentDownloader: %v", err)
	}
	defer dl.Shutdown(context.Background())

	task := &Task{InfoHash: "abc123def456abc123def456abc123def456abc1"}
	ok, err := dl.Available(context.Background(), task)
	if err != nil {
		t.Fatalf("Available: %v", err)
	}
	if !ok {
		t.Error("Available() = false, want true when infoHash is set")
	}
}

// TestTorrentDownloader_Available_WithoutInfoHash verifica que Available() devuelve
// false cuando la tarea no tiene infoHash.
func TestTorrentDownloader_Available_WithoutInfoHash(t *testing.T) {
	dir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("NewTorrentDownloader: %v", err)
	}
	defer dl.Shutdown(context.Background())

	task := &Task{InfoHash: ""}
	ok, err := dl.Available(context.Background(), task)
	if err != nil {
		t.Fatalf("Available: %v", err)
	}
	if ok {
		t.Error("Available() = true, want false when infoHash is empty")
	}
}

// TestTorrentDownloader_Shutdown_Clean verifica que Shutdown() no genera panics
// ni errores inesperados.
func TestTorrentDownloader_Shutdown_Clean(t *testing.T) {
	dir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("NewTorrentDownloader: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := dl.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown() error = %v", err)
	}
}

// TestTorrentDownloader_Cancel_NonExistent verifica que Cancel() no genera panic
// para un ID de tarea que no existe.
func TestTorrentDownloader_Cancel_NonExistent(t *testing.T) {
	dir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("NewTorrentDownloader: %v", err)
	}
	defer dl.Shutdown(context.Background())

	// No debe hacer panic
	if err := dl.Cancel("nonexistent-task-id"); err != nil {
		t.Errorf("Cancel() unexpected error: %v", err)
	}
}

// TestTorrentDownloader_Pause_NonExistent verifica que Pause() no genera panic
// para un ID de tarea que no existe.
func TestTorrentDownloader_Pause_NonExistent(t *testing.T) {
	dir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{DataDir: dir})
	if err != nil {
		t.Fatalf("NewTorrentDownloader: %v", err)
	}
	defer dl.Shutdown(context.Background())

	if err := dl.Pause("nonexistent-task-id"); err != nil {
		t.Errorf("Pause() unexpected error: %v", err)
	}
}

// TestTorrentDownloader_StallTimeout_Default verifica que StallTimeout se inicializa
// con el valor por defecto (30m) cuando se pasa 0.
func TestTorrentDownloader_StallTimeout_Default(t *testing.T) {
	dir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{
		DataDir:      dir,
		StallTimeout: 0, // debe usar el default 30m
	})
	if err != nil {
		t.Fatalf("NewTorrentDownloader: %v", err)
	}
	defer dl.Shutdown(context.Background())

	if dl.cfg.StallTimeout != 30*time.Minute {
		t.Errorf("StallTimeout = %v, want 30m", dl.cfg.StallTimeout)
	}
}

// TestTorrentDownloader_StallTimeout_Custom verifica que un StallTimeout personalizado
// se respeta sin ser sobreescrito.
func TestTorrentDownloader_StallTimeout_Custom(t *testing.T) {
	dir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{
		DataDir:      dir,
		StallTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewTorrentDownloader: %v", err)
	}
	defer dl.Shutdown(context.Background())

	if dl.cfg.StallTimeout != 5*time.Minute {
		t.Errorf("StallTimeout = %v, want 5m", dl.cfg.StallTimeout)
	}
}

// TestTorrentDownloader_SeedDisabled verifica que cuando SeedEnabled=false,
// el downloader se crea correctamente (NoUpload implícito).
func TestTorrentDownloader_SeedDisabled(t *testing.T) {
	dir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{
		DataDir:     dir,
		SeedEnabled: false,
	})
	if err != nil {
		t.Fatalf("NewTorrentDownloader: %v", err)
	}
	defer dl.Shutdown(context.Background())

	if dl.cfg.SeedEnabled {
		t.Error("SeedEnabled should be false")
	}
}

// TestTorrentDownloader_SeedEnabled verifica que cuando SeedEnabled=true,
// el downloader se crea correctamente.
func TestTorrentDownloader_SeedEnabled(t *testing.T) {
	dir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{
		DataDir:     dir,
		SeedEnabled: true,
	})
	if err != nil {
		t.Fatalf("NewTorrentDownloader: %v", err)
	}
	defer dl.Shutdown(context.Background())

	if !dl.cfg.SeedEnabled {
		t.Error("SeedEnabled should be true")
	}
}

// TestTorrentDownloader_RateLimiting_Download verifica que crear un downloader
// con MaxDownloadRate > 0 no devuelve error.
func TestTorrentDownloader_RateLimiting_Download(t *testing.T) {
	dir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{
		DataDir:         dir,
		MaxDownloadRate: 5 * 1024 * 1024, // 5 MB/s
	})
	if err != nil {
		t.Fatalf("NewTorrentDownloader with download rate limit: %v", err)
	}
	defer dl.Shutdown(context.Background())

	if dl.cfg.MaxDownloadRate != 5*1024*1024 {
		t.Errorf("MaxDownloadRate = %d, want %d", dl.cfg.MaxDownloadRate, 5*1024*1024)
	}
}

// TestTorrentDownloader_RateLimiting_Upload verifica que crear un downloader
// con MaxUploadRate > 0 no devuelve error.
func TestTorrentDownloader_RateLimiting_Upload(t *testing.T) {
	dir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{
		DataDir:       dir,
		MaxUploadRate: 1 * 1024 * 1024, // 1 MB/s
	})
	if err != nil {
		t.Fatalf("NewTorrentDownloader with upload rate limit: %v", err)
	}
	defer dl.Shutdown(context.Background())

	if dl.cfg.MaxUploadRate != 1*1024*1024 {
		t.Errorf("MaxUploadRate = %d, want %d", dl.cfg.MaxUploadRate, 1*1024*1024)
	}
}

// TestTorrentDownloader_DownloadTimeout_MetadataCancel verifica que Download()
// respeta la cancelación de contexto durante la espera de metadata.
// No hay red real, así que el timeout de contexto debe terminar la operación.
func TestTorrentDownloader_DownloadTimeout_MetadataCancel(t *testing.T) {
	dir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{
		DataDir:         dir,
		MetadataTimeout: 100 * time.Millisecond, // muy corto para que falle rápido
	})
	if err != nil {
		t.Fatalf("NewTorrentDownloader: %v", err)
	}
	defer dl.Shutdown(context.Background())

	task := &Task{
		ID:       "timeout-test-1234567890123456",
		InfoHash: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		Title:    "Non-existent Torrent",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	progressCh := make(chan Progress, 16)
	_, err = dl.Download(ctx, task, dir, progressCh)
	close(progressCh)

	if err == nil {
		t.Error("expected error when metadata timeout with no peers")
	}
}

// TestTorrentDownloader_ImplementsInterface verifica en tiempo de compilación
// que *TorrentDownloader implementa la interfaz Downloader.
func TestTorrentDownloader_ImplementsInterface(t *testing.T) {
	var _ Downloader = (*TorrentDownloader)(nil)
}
