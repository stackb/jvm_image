package jartar

import (
	"archive/tar"
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// createTestJar creates a jar (zip) file at the given path with the specified entries.
// Each entry is a name->content pair. Names ending in "/" are directories.
func createTestJar(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if content != "" {
			if _, err := w.Write([]byte(content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// readTar reads a tar file and returns a map of entry name -> content.
func readTar(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	result := make(map[string]string)
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			result[hdr.Name] = string(data)
		} else {
			result[hdr.Name] = ""
		}
	}
	return result
}

func TestSplit_NoLayers(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "test.jar")
	fallbackPath := filepath.Join(dir, "fallback.tar")

	createTestJar(t, jarPath, map[string]string{
		"META-INF/MANIFEST.MF": "Manifest-Version: 1.0\n",
		"com/example/Main.class": "main-class-bytes",
	})

	if err := Split(jarPath, fallbackPath, nil); err != nil {
		t.Fatal(err)
	}

	entries := readTar(t, fallbackPath)
	if _, ok := entries["META-INF/MANIFEST.MF"]; !ok {
		t.Error("expected META-INF/MANIFEST.MF in fallback tar")
	}
	if _, ok := entries["com/example/Main.class"]; !ok {
		t.Error("expected com/example/Main.class in fallback tar")
	}
}

func TestSplit_WithLayers(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "test.jar")
	fallbackPath := filepath.Join(dir, "fallback.tar")
	googleLayerPath := filepath.Join(dir, "google.tar")
	exampleLayerPath := filepath.Join(dir, "example.tar")

	createTestJar(t, jarPath, map[string]string{
		"META-INF/MANIFEST.MF":              "Manifest-Version: 1.0\n",
		"com/google/common/collect/Lists.class": "google-class-bytes",
		"com/google/common/base/Strings.class":  "google-strings-bytes",
		"com/example/Main.class":                "main-class-bytes",
		"org/other/Lib.class":                   "other-bytes",
	})

	layers := []Layer{
		{Prefix: "com/google/", OutputPath: googleLayerPath},
		{Prefix: "com/example/", OutputPath: exampleLayerPath},
	}

	if err := Split(jarPath, fallbackPath, layers); err != nil {
		t.Fatal(err)
	}

	// Check google layer.
	googleEntries := readTar(t, googleLayerPath)
	if len(googleEntries) != 2 {
		t.Errorf("google layer: got %d entries, want 2", len(googleEntries))
	}
	if _, ok := googleEntries["com/google/common/collect/Lists.class"]; !ok {
		t.Error("expected Lists.class in google layer")
	}
	if _, ok := googleEntries["com/google/common/base/Strings.class"]; !ok {
		t.Error("expected Strings.class in google layer")
	}

	// Check example layer.
	exampleEntries := readTar(t, exampleLayerPath)
	if len(exampleEntries) != 1 {
		t.Errorf("example layer: got %d entries, want 1", len(exampleEntries))
	}
	if _, ok := exampleEntries["com/example/Main.class"]; !ok {
		t.Error("expected Main.class in example layer")
	}

	// Check fallback (META-INF + org/other).
	fallbackEntries := readTar(t, fallbackPath)
	if _, ok := fallbackEntries["META-INF/MANIFEST.MF"]; !ok {
		t.Error("expected MANIFEST.MF in fallback tar")
	}
	if _, ok := fallbackEntries["org/other/Lib.class"]; !ok {
		t.Error("expected Lib.class in fallback tar")
	}
	// Ensure no google or example entries leaked to fallback.
	for name := range fallbackEntries {
		if name == "com/google/common/collect/Lists.class" ||
			name == "com/google/common/base/Strings.class" ||
			name == "com/example/Main.class" {
			t.Errorf("unexpected entry %s in fallback tar", name)
		}
	}
}

func TestSplit_EmptyLayers(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "test.jar")
	fallbackPath := filepath.Join(dir, "fallback.tar")
	emptyLayerPath := filepath.Join(dir, "empty.tar")

	createTestJar(t, jarPath, map[string]string{
		"com/example/Main.class": "main-class-bytes",
	})

	layers := []Layer{
		{Prefix: "org/nonexistent/", OutputPath: emptyLayerPath},
	}

	if err := Split(jarPath, fallbackPath, layers); err != nil {
		t.Fatal(err)
	}

	// Empty layer tar should exist and be readable (just empty).
	emptyEntries := readTar(t, emptyLayerPath)
	if len(emptyEntries) != 0 {
		t.Errorf("empty layer: got %d entries, want 0", len(emptyEntries))
	}

	// Everything goes to fallback.
	fallbackEntries := readTar(t, fallbackPath)
	if _, ok := fallbackEntries["com/example/Main.class"]; !ok {
		t.Error("expected Main.class in fallback tar")
	}
}

func TestSplit_FirstMatchWins(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "test.jar")
	fallbackPath := filepath.Join(dir, "fallback.tar")
	broadLayerPath := filepath.Join(dir, "broad.tar")
	narrowLayerPath := filepath.Join(dir, "narrow.tar")

	createTestJar(t, jarPath, map[string]string{
		"com/google/common/base/Strings.class": "strings-bytes",
	})

	// Broad prefix listed first should win.
	layers := []Layer{
		{Prefix: "com/", OutputPath: broadLayerPath},
		{Prefix: "com/google/", OutputPath: narrowLayerPath},
	}

	if err := Split(jarPath, fallbackPath, layers); err != nil {
		t.Fatal(err)
	}

	broadEntries := readTar(t, broadLayerPath)
	if _, ok := broadEntries["com/google/common/base/Strings.class"]; !ok {
		t.Error("expected Strings.class in broad layer (first match wins)")
	}

	narrowEntries := readTar(t, narrowLayerPath)
	if len(narrowEntries) != 0 {
		t.Errorf("narrow layer: got %d entries, want 0 (broad should have won)", len(narrowEntries))
	}
}
