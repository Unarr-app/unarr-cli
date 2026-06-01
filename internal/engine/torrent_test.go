package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMakeReadable_FixesZeroMode verifies makeReadable turns an unreadable
// mode-0000 file (the anacrolix mmap default) into a readable 0644 one.
func TestMakeReadable_FixesZeroMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(p, []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(p); err == nil {
		f.Close()
		t.Skip("running as root — 0000 files are readable; can't exercise the fix")
	}
	makeReadable(p)
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("file still unreadable after makeReadable: %v", err)
	}
	f.Close()
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %o, want 0644", fi.Mode().Perm())
	}
}

// TestMakeReadable_DirWalk verifies the directory branch relaxes a 0000 file
// nested inside the download dir.
func TestMakeReadable_DirWalk(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "Release")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(sub, "movie.mkv")
	if err := os.WriteFile(p, []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(p); err == nil {
		f.Close()
		t.Skip("running as root — 0000 files are readable")
	}
	makeReadable(sub)
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("nested file unreadable after makeReadable: %v", err)
	}
	f.Close()
}

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

// TestSeedTargetReached cubre la lógica pura de parada del seeding: ratio,
// tiempo, ninguno, ambos (el primero que se cumple gana) y la guarda de tamaño
// cero (no debe dividir por cero ni parar por ratio).
func TestSeedTargetReached(t *testing.T) {
	tests := []struct {
		name        string
		ratioTarget float64
		timeTarget  time.Duration
		uploaded    int64
		size        int64
		elapsed     time.Duration
		wantStop    bool
	}{
		{"ratio reached", 2.0, 0, 200, 100, time.Minute, true},
		{"ratio not reached", 2.0, 0, 150, 100, time.Minute, false},
		{"ratio exactly met", 1.0, 0, 100, 100, time.Minute, true},
		{"time reached", 0, time.Hour, 10, 100, 2 * time.Hour, true},
		{"time not reached", 0, time.Hour, 10, 100, 30 * time.Minute, false},
		{"no targets never stops", 0, 0, 9999, 100, 99 * time.Hour, false},
		{"ratio wins when both set", 2.0, time.Hour, 200, 100, time.Second, true},
		{"time wins when ratio short", 5.0, time.Hour, 100, 100, 2 * time.Hour, true},
		{"zero size guards div", 2.0, 0, 200, 0, time.Minute, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reason := seedTargetReached(tc.ratioTarget, tc.timeTarget, tc.uploaded, tc.size, tc.elapsed)
			if got := reason != ""; got != tc.wantStop {
				t.Errorf("seedTargetReached(ratio=%.1f time=%s up=%d size=%d el=%s) stop=%v (reason %q), want %v",
					tc.ratioTarget, tc.timeTarget, tc.uploaded, tc.size, tc.elapsed, got, reason, tc.wantStop)
			}
		})
	}
}

// TestTorrentDownloader_SeedRatioTime verifica que SeedRatio y SeedTime se
// propagan a la config del downloader.
func TestTorrentDownloader_SeedRatioTime(t *testing.T) {
	dir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{
		DataDir:     dir,
		SeedEnabled: true,
		SeedRatio:   1.5,
		SeedTime:    2 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewTorrentDownloader: %v", err)
	}
	defer dl.Shutdown(context.Background())

	if dl.cfg.SeedRatio != 1.5 {
		t.Errorf("SeedRatio = %v, want 1.5", dl.cfg.SeedRatio)
	}
	if dl.cfg.SeedTime != 2*time.Hour {
		t.Errorf("SeedTime = %v, want 2h", dl.cfg.SeedTime)
	}
	if dl.seedCtx == nil || dl.seedCancel == nil {
		t.Error("seedCtx/seedCancel must be initialised by the constructor")
	}
}

// TestSeedAndDrop_NoTargetReturnsImmediately verifica que sin ratio ni tiempo
// objetivo, seedAndDrop retorna de inmediato (siembra indefinida) sin tocar el
// handle — por eso es seguro pasar un torrent nil.
func TestSeedAndDrop_NoTargetReturnsImmediately(t *testing.T) {
	dir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{DataDir: dir, SeedEnabled: true}) // ratio 0, time 0
	if err != nil {
		t.Fatalf("NewTorrentDownloader: %v", err)
	}
	defer dl.Shutdown(context.Background())

	done := make(chan struct{})
	go func() {
		dl.seedAndDrop("no-target-task-id", nil, 1000)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("seedAndDrop with no target should return immediately")
	}
}

// TestSeedAndDrop_StopsOnSeedCtxCancel verifica que seedAndDrop sale cuando se
// cancela seedCtx (ruta de Shutdown), incluso con un objetivo de ratio alto y el
// tick deshabilitado — el único camino de salida es seedCtx.Done().
func TestSeedAndDrop_StopsOnSeedCtxCancel(t *testing.T) {
	dir := t.TempDir()
	dl, err := NewTorrentDownloader(TorrentConfig{DataDir: dir, SeedEnabled: true, SeedRatio: 99})
	if err != nil {
		t.Fatalf("NewTorrentDownloader: %v", err)
	}
	defer dl.Shutdown(context.Background())

	dl.seedCheckInterval = time.Hour // el ticker no disparará; solo seedCtx.Done() puede terminar
	dl.seedCancel()                  // cancela antes de arrancar el monitor

	done := make(chan struct{})
	go func() {
		dl.seedAndDrop("ctx-cancel-task-id", nil, 1000)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("seedAndDrop should return when seedCtx is cancelled")
	}
}
