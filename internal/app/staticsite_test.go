package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// writeFiles creates each name (slash-separated, relative to dir) with the
// given contents, making parent directories as needed.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, contents := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func serveStatic(dir, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	NewStaticHandler(dir).ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestNewStaticHandler_ServesAFile(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"index.html":    "<html>app</html>",
		"assets/app.js": "console.log(1)",
	})

	rec := serveStatic(dir, http.MethodGet, "/assets/app.js")
	if rec.Code != http.StatusOK || rec.Body.String() != "console.log(1)" {
		t.Fatalf("GET /assets/app.js = %d %q, want 200 with the file's content", rec.Code, rec.Body.String())
	}
}

func TestNewStaticHandler_ServesIndexAtTheRoot(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"index.html": "<html>app</html>"})

	rec := serveStatic(dir, http.MethodGet, "/")
	if rec.Code != http.StatusOK || rec.Body.String() != "<html>app</html>" {
		t.Fatalf("GET / = %d %q, want 200 with index.html's content", rec.Code, rec.Body.String())
	}
}

func TestNewStaticHandler_FallsBackToIndexForAnUnknownPath(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"index.html":    "<html>app</html>",
		"assets/app.js": "console.log(1)",
	})

	for _, target := range []string{"/admin", "/admin/", "/no/such/page", "/assets", "/assets/"} {
		rec := serveStatic(dir, http.MethodGet, target)
		if rec.Code != http.StatusOK || rec.Body.String() != "<html>app</html>" {
			t.Errorf("GET %s = %d %q, want 200 with index.html's content", target, rec.Code, rec.Body.String())
		}
	}
}

func TestNewStaticHandler_DoesNotEscapeTheDirectory(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "dist")
	writeFiles(t, parent, map[string]string{
		"secret.txt":      "secret",
		"dist/index.html": "<html>app</html>",
	})

	for _, target := range []string{"/../secret.txt", "/%2e%2e/secret.txt", "/assets/../../secret.txt"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.URL.Path = target // bypass NewRequest's own cleaning
		req.URL.RawPath = ""
		rec := httptest.NewRecorder()
		NewStaticHandler(dir).ServeHTTP(rec, req)
		if rec.Body.String() == "secret" {
			t.Errorf("GET %s served a file outside the directory", target)
		}
	}
}

func TestNewStaticHandler_UnreadableFileIsAnErrorNotTheFallback(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any file")
	}
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"index.html": "<html>app</html>",
		"app.js":     "console.log(1)",
	})
	if err := os.Chmod(filepath.Join(dir, "app.js"), 0); err != nil {
		t.Fatal(err)
	}

	rec := serveStatic(dir, http.MethodGet, "/app.js")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET /app.js (unreadable) = %d %q, want 500", rec.Code, rec.Body.String())
	}
}

func TestNewStaticHandler_MissingIndexIsA404NotAPanic(t *testing.T) {
	// An empty directory, as when the frontend has never been built.
	dir := t.TempDir()

	for _, target := range []string{"/", "/admin"} {
		rec := serveStatic(dir, http.MethodGet, target)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s with no index.html = %d %q, want 404", target, rec.Code, rec.Body.String())
		}
	}
}

func TestNewStaticHandler_MissingDirectoryIsA404(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never-built")

	rec := serveStatic(dir, http.MethodGet, "/")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET / with no directory = %d %q, want 404", rec.Code, rec.Body.String())
	}
}
