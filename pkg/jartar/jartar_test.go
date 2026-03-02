package jartar

import (
	"archive/tar"
	"archive/zip"
	"encoding/json"
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

// createLockFile creates a maven lock file JSON with the given packages map.
func createLockFile(t *testing.T, path string, packages map[string][]string) {
	t.Helper()
	lf := MavenLockFile{Packages: packages}
	data, err := json.Marshal(lf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestSplit_NoLayers(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "test.jar")
	fallbackPath := filepath.Join(dir, "fallback.tar")

	createTestJar(t, jarPath, map[string]string{
		"META-INF/MANIFEST.MF":  "Manifest-Version: 1.0\n",
		"com/example/Main.class": "main-class-bytes",
	})

	if err := Split(SplitOptions{
		InputPath:    jarPath,
		FallbackPath: fallbackPath,
	}); err != nil {
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
		"META-INF/MANIFEST.MF":                 "Manifest-Version: 1.0\n",
		"com/google/common/collect/Lists.class": "google-class-bytes",
		"com/google/common/base/Strings.class":  "google-strings-bytes",
		"com/example/Main.class":                "main-class-bytes",
		"org/other/Lib.class":                   "other-bytes",
	})

	if err := Split(SplitOptions{
		InputPath:    jarPath,
		FallbackPath: fallbackPath,
		Layers: []Layer{
			{Prefix: "com/google/", OutputPath: googleLayerPath},
			{Prefix: "com/example/", OutputPath: exampleLayerPath},
		},
	}); err != nil {
		t.Fatal(err)
	}

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

	exampleEntries := readTar(t, exampleLayerPath)
	if len(exampleEntries) != 1 {
		t.Errorf("example layer: got %d entries, want 1", len(exampleEntries))
	}
	if _, ok := exampleEntries["com/example/Main.class"]; !ok {
		t.Error("expected Main.class in example layer")
	}

	fallbackEntries := readTar(t, fallbackPath)
	if _, ok := fallbackEntries["META-INF/MANIFEST.MF"]; !ok {
		t.Error("expected MANIFEST.MF in fallback tar")
	}
	if _, ok := fallbackEntries["org/other/Lib.class"]; !ok {
		t.Error("expected Lib.class in fallback tar")
	}
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

	if err := Split(SplitOptions{
		InputPath:    jarPath,
		FallbackPath: fallbackPath,
		Layers: []Layer{
			{Prefix: "org/nonexistent/", OutputPath: emptyLayerPath},
		},
	}); err != nil {
		t.Fatal(err)
	}

	emptyEntries := readTar(t, emptyLayerPath)
	if len(emptyEntries) != 0 {
		t.Errorf("empty layer: got %d entries, want 0", len(emptyEntries))
	}

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

	if err := Split(SplitOptions{
		InputPath:    jarPath,
		FallbackPath: fallbackPath,
		Layers: []Layer{
			{Prefix: "com/", OutputPath: broadLayerPath},
			{Prefix: "com/google/", OutputPath: narrowLayerPath},
		},
	}); err != nil {
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

func TestSplit_ArtifactLayers(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "test.jar")
	fallbackPath := filepath.Join(dir, "fallback.tar")
	guavaPath := filepath.Join(dir, "guava.tar")
	jsr305Path := filepath.Join(dir, "jsr305.tar")
	lockFilePath := filepath.Join(dir, "lock.json")

	createTestJar(t, jarPath, map[string]string{
		"META-INF/MANIFEST.MF":                 "Manifest-Version: 1.0\n",
		"com/google/common/collect/Lists.class": "lists-bytes",
		"com/google/common/base/Strings.class":  "strings-bytes",
		"javax/annotation/Nonnull.class":        "nonnull-bytes",
		"example/Main.class":                    "main-bytes",
	})

	createLockFile(t, lockFilePath, map[string][]string{
		"com.google.guava:guava": {
			"com.google.common.collect",
			"com.google.common.base",
		},
		"com.google.code.findbugs:jsr305": {
			"javax.annotation",
		},
	})

	if err := Split(SplitOptions{
		InputPath:         jarPath,
		FallbackPath:      fallbackPath,
		MavenLockFilePath: lockFilePath,
		Artifacts: []Artifact{
			{ID: "com.google.guava:guava", OutputPath: guavaPath},
			{ID: "com.google.code.findbugs:jsr305", OutputPath: jsr305Path},
		},
	}); err != nil {
		t.Fatal(err)
	}

	guavaEntries := readTar(t, guavaPath)
	if _, ok := guavaEntries["com/google/common/collect/Lists.class"]; !ok {
		t.Error("expected Lists.class in guava layer")
	}
	if _, ok := guavaEntries["com/google/common/base/Strings.class"]; !ok {
		t.Error("expected Strings.class in guava layer")
	}
	if len(guavaEntries) != 2 {
		t.Errorf("guava layer: got %d entries, want 2: %v", len(guavaEntries), guavaEntries)
	}

	jsr305Entries := readTar(t, jsr305Path)
	if _, ok := jsr305Entries["javax/annotation/Nonnull.class"]; !ok {
		t.Error("expected Nonnull.class in jsr305 layer")
	}
	if len(jsr305Entries) != 1 {
		t.Errorf("jsr305 layer: got %d entries, want 1: %v", len(jsr305Entries), jsr305Entries)
	}

	fallbackEntries := readTar(t, fallbackPath)
	if _, ok := fallbackEntries["META-INF/MANIFEST.MF"]; !ok {
		t.Error("expected MANIFEST.MF in fallback")
	}
	if _, ok := fallbackEntries["example/Main.class"]; !ok {
		t.Error("expected Main.class in fallback")
	}
	if len(fallbackEntries) != 2 {
		t.Errorf("fallback: got %d entries, want 2: %v", len(fallbackEntries), fallbackEntries)
	}
}

func TestSplit_ExplicitLayersTakePriorityOverArtifacts(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "test.jar")
	fallbackPath := filepath.Join(dir, "fallback.tar")
	explicitLayerPath := filepath.Join(dir, "explicit.tar")
	artifactPath := filepath.Join(dir, "artifact.tar")
	lockFilePath := filepath.Join(dir, "lock.json")

	createTestJar(t, jarPath, map[string]string{
		"com/google/common/collect/Lists.class": "lists-bytes",
	})

	createLockFile(t, lockFilePath, map[string][]string{
		"com.google.guava:guava": {
			"com.google.common.collect",
		},
	})

	// Explicit layer with broader prefix should win over artifact.
	if err := Split(SplitOptions{
		InputPath:         jarPath,
		FallbackPath:      fallbackPath,
		MavenLockFilePath: lockFilePath,
		Layers: []Layer{
			{Prefix: "com/google/", OutputPath: explicitLayerPath},
		},
		Artifacts: []Artifact{
			{ID: "com.google.guava:guava", OutputPath: artifactPath},
		},
	}); err != nil {
		t.Fatal(err)
	}

	explicitEntries := readTar(t, explicitLayerPath)
	if _, ok := explicitEntries["com/google/common/collect/Lists.class"]; !ok {
		t.Error("expected Lists.class in explicit layer (explicit takes priority)")
	}

	artifactEntries := readTar(t, artifactPath)
	if len(artifactEntries) != 0 {
		t.Errorf("artifact layer: got %d entries, want 0 (explicit should have won)", len(artifactEntries))
	}
}

func TestSplit_ArtifactNotInLockFile(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "test.jar")
	fallbackPath := filepath.Join(dir, "fallback.tar")
	artifactPath := filepath.Join(dir, "artifact.tar")
	lockFilePath := filepath.Join(dir, "lock.json")

	createTestJar(t, jarPath, map[string]string{
		"com/example/Main.class": "main-bytes",
	})

	// Lock file has no packages for this artifact.
	createLockFile(t, lockFilePath, map[string][]string{})

	if err := Split(SplitOptions{
		InputPath:         jarPath,
		FallbackPath:      fallbackPath,
		MavenLockFilePath: lockFilePath,
		Artifacts: []Artifact{
			{ID: "com.unknown:lib", OutputPath: artifactPath},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Artifact tar should be empty (no packages matched).
	artifactEntries := readTar(t, artifactPath)
	if len(artifactEntries) != 0 {
		t.Errorf("artifact layer: got %d entries, want 0", len(artifactEntries))
	}

	// Everything goes to fallback.
	fallbackEntries := readTar(t, fallbackPath)
	if _, ok := fallbackEntries["com/example/Main.class"]; !ok {
		t.Error("expected Main.class in fallback")
	}
}
