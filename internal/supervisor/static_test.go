package supervisor

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSite(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"index.html":      "<h1>home</h1>",
		"about.html":      "<p>about</p>",
		"assets/app.js":   "console.log(1)",
		"docs/index.html": "<p>docs</p>",
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A secret next to the served folder must never be reachable.
	if err := os.WriteFile(filepath.Join(filepath.Dir(root), "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://site"+path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	return rec.Code, string(body)
}

func TestStaticHandler(t *testing.T) {
	root := writeSite(t)
	tests := []struct {
		name     string
		spa      bool
		path     string
		wantCode int
		wantBody string
	}{
		{"root serves index", false, "/", 200, "home"},
		{"existing file", false, "/about.html", 200, "about"},
		{"nested asset", false, "/assets/app.js", 200, "console.log"},
		{"directory index", false, "/docs/", 200, "docs"},
		{"unknown path without spa", false, "/dashboard/settings", 404, ""},
		{"unknown path with spa falls back", true, "/dashboard/settings", 200, "home"},
		{"existing file with spa", true, "/about.html", 200, "about"},
		{"missing asset with spa falls back", true, "/assets/missing.js", 200, "home"},
		{"traversal without spa", false, "/../secret.txt", 404, ""},
		{"traversal with spa is rejected", true, "/../secret.txt", 400, ""},
		{"encoded traversal is rejected", true, "/%2e%2e/secret.txt", 400, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, body := get(t, staticHandler(root, tc.spa), tc.path)
			if code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %q)", code, tc.wantCode, body)
			}
			if !strings.Contains(body, tc.wantBody) {
				t.Fatalf("body = %q, want it to contain %q", body, tc.wantBody)
			}
			if strings.Contains(body, "secret") {
				t.Fatal("served a file outside the static root")
			}
		})
	}
}

func TestStaticHandlerSPAFallbackIsNotCached(t *testing.T) {
	root := writeSite(t)
	req := httptest.NewRequest(http.MethodGet, "http://site/some/route", nil)
	rec := httptest.NewRecorder()
	staticHandler(root, true).ServeHTTP(rec, req)
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache so new deploys are picked up", got)
	}
}
