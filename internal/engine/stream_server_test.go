package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// readSeekNopCloser envuelve un strings.Reader como ReadSeekCloser.
type readSeekNopCloser struct {
	*strings.Reader
}

func (r *readSeekNopCloser) Close() error { return nil }

func newFakeProvider(name string, content []byte) FileProvider {
	return &fakeFileProviderSeekable{name: name, content: content}
}

// fakeFileProviderSeekable implementa FileProvider con un reader buscable.
type fakeFileProviderSeekable struct {
	name    string
	content []byte
}

func (f *fakeFileProviderSeekable) FileName() string { return f.name }
func (f *fakeFileProviderSeekable) FileSize() int64  { return int64(len(f.content)) }
func (f *fakeFileProviderSeekable) NewFileReader(_ context.Context) io.ReadSeekCloser {
	return &readSeekNopCloser{strings.NewReader(string(f.content))}
}

// TestStreamServer_Listen_BindsPort verifica que Listen() enlaza a un puerto
// y URL() devuelve una URL accesible.
func TestStreamServer_Listen_BindsPort(t *testing.T) {
	srv := NewStreamServer(0) // puerto aleatorio
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer srv.Shutdown(context.Background())

	url := srv.URL()
	if url == "" {
		t.Fatal("URL() returned empty string after Listen()")
	}
	if !strings.HasPrefix(url, "http://") {
		t.Errorf("URL() = %q, want http:// prefix", url)
	}
	if srv.Port() == 0 {
		t.Error("Port() should be non-zero after Listen()")
	}
}

// TestStreamServer_Listen_RandomPort verifica que port=0 asigna un puerto disponible.
func TestStreamServer_Listen_RandomPort(t *testing.T) {
	srv := NewStreamServer(0)
	ctx := context.Background()

	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer srv.Shutdown(ctx)

	port := srv.Port()
	if port <= 0 || port > 65535 {
		t.Errorf("Port() = %d, want valid port 1-65535", port)
	}
}

// TestStreamServer_URL_Format verifica que la URL tiene el formato correcto
// con host y puerto.
func TestStreamServer_URL_Format(t *testing.T) {
	srv := NewStreamServer(0)
	ctx := context.Background()

	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer srv.Shutdown(ctx)

	url := srv.URL()
	port := srv.Port()

	expectedSuffix := fmt.Sprintf(":%d/stream", port)
	if !strings.Contains(url, expectedSuffix) {
		t.Errorf("URL() = %q, want to contain %q", url, expectedSuffix)
	}
}

// TestStreamServer_HasFile verifica que HasFile() refleja el estado correcto.
func TestStreamServer_HasFile(t *testing.T) {
	srv := NewStreamServer(0)
	ctx := context.Background()

	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer srv.Shutdown(ctx)

	if srv.HasFile() {
		t.Error("HasFile() = true before SetFile(), want false")
	}

	provider := newFakeProvider("test.mkv", []byte("fake video content"))
	srv.SetFile(provider, "task-123")

	if !srv.HasFile() {
		t.Error("HasFile() = false after SetFile(), want true")
	}

	if srv.CurrentTaskID() != "task-123" {
		t.Errorf("CurrentTaskID() = %q, want task-123", srv.CurrentTaskID())
	}
}

// TestStreamServer_ClearFile verifica que ClearFile() elimina el provider actual.
func TestStreamServer_ClearFile(t *testing.T) {
	srv := NewStreamServer(0)
	ctx := context.Background()

	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer srv.Shutdown(ctx)

	provider := newFakeProvider("video.mkv", []byte("content"))
	srv.SetFile(provider, "task-xyz")

	srv.ClearFile()

	if srv.HasFile() {
		t.Error("HasFile() = true after ClearFile(), want false")
	}
	if srv.CurrentTaskID() != "" {
		t.Errorf("CurrentTaskID() = %q, want empty after ClearFile()", srv.CurrentTaskID())
	}
}

// TestStreamServer_NoFile_Returns404 verifica que sin archivo configurado
// el servidor devuelve 404.
func TestStreamServer_NoFile_Returns404(t *testing.T) {
	srv := NewStreamServer(0)
	ctx := context.Background()

	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer srv.Shutdown(ctx)

	resp, err := http.Get(srv.URL())
	if err != nil {
		t.Fatalf("GET %s: %v", srv.URL(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when no file set", resp.StatusCode)
	}
}

// TestStreamServer_WithFile_Returns200 verifica que con archivo configurado
// el servidor sirve el contenido correctamente.
func TestStreamServer_WithFile_Returns200(t *testing.T) {
	content := []byte("fake video bytes for testing")
	srv := NewStreamServer(0)
	ctx := context.Background()

	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer srv.Shutdown(ctx)

	provider := newFakeProvider("movie.mkv", content)
	srv.SetFile(provider, "task-abc")

	resp, err := http.Get(srv.URL())
	if err != nil {
		t.Fatalf("GET %s: %v", srv.URL(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		t.Error("response body is empty, expected file content")
	}
}

// TestStreamServer_Shutdown_ReleasesPort verifica que después de Shutdown()
// el servidor no sigue respondiendo.
func TestStreamServer_Shutdown_ReleasesPort(t *testing.T) {
	srv := NewStreamServer(0)
	ctx := context.Background()

	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("Listen() error: %v", err)
	}

	url := srv.URL()

	// Verificar que funciona antes de Shutdown
	provider := newFakeProvider("test.mkv", []byte("data"))
	srv.SetFile(provider, "t1")
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET before shutdown: %v", err)
	}
	resp.Body.Close()

	// Shutdown
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Errorf("Shutdown() error: %v", err)
	}

	// Después de shutdown, las conexiones deben fallar
	client := &http.Client{Timeout: 500 * time.Millisecond}
	if resp2, getErr := client.Get(url); getErr == nil {
		resp2.Body.Close()
		t.Error("expected error after Shutdown(), server should not be accessible")
	}
}

// TestStreamServer_Concurrent verifica que múltiples requests concurrentes
// son manejados correctamente.
func TestStreamServer_Concurrent(t *testing.T) {
	content := []byte("streaming content for concurrent access")
	srv := NewStreamServer(0)
	ctx := context.Background()

	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer srv.Shutdown(ctx)

	provider := newFakeProvider("concurrent.mkv", content)
	srv.SetFile(provider, "task-concurrent")

	const numRequests = 5
	var wg sync.WaitGroup
	errors := make([]error, numRequests)

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			resp, err := http.Get(srv.URL())
			if err != nil {
				errors[idx] = err
				return
			}
			defer resp.Body.Close()
			io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				errors[idx] = fmt.Errorf("request %d: status %d", idx, resp.StatusCode)
			}
		}(i)
	}

	wg.Wait()

	for i, err := range errors {
		if err != nil {
			t.Errorf("concurrent request %d failed: %v", i, err)
		}
	}
}

// TestStreamServer_SetFile_SwapsProvider verifica que SetFile() reemplaza
// el provider anterior correctamente.
func TestStreamServer_SetFile_SwapsProvider(t *testing.T) {
	srv := NewStreamServer(0)
	ctx := context.Background()

	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer srv.Shutdown(ctx)

	// Primer archivo
	p1 := newFakeProvider("first.mkv", []byte("first content"))
	srv.SetFile(p1, "task-1")

	if srv.CurrentTaskID() != "task-1" {
		t.Errorf("after first SetFile: taskID = %q, want task-1", srv.CurrentTaskID())
	}

	// Swap a segundo archivo
	p2 := newFakeProvider("second.mkv", []byte("second content"))
	srv.SetFile(p2, "task-2")

	if srv.CurrentTaskID() != "task-2" {
		t.Errorf("after second SetFile: taskID = %q, want task-2", srv.CurrentTaskID())
	}
}

// TestStreamServer_MKV_ContentType verifica que el Content-Type para .mkv
// es el correcto.
func TestStreamServer_MKV_ContentType(t *testing.T) {
	srv := NewStreamServer(0)
	ctx := context.Background()

	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer srv.Shutdown(ctx)

	provider := newFakeProvider("movie.mkv", []byte("mkv content"))
	srv.SetFile(provider, "task-mkv")

	resp, err := http.Get(srv.URL())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "matroska") && !strings.Contains(ct, "mkv") {
		t.Errorf("Content-Type = %q, want matroska/mkv MIME type", ct)
	}
}
