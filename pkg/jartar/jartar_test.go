package jartar

import (
	"archive/tar"
	"archive/zip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
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

	if _, err := Split(SplitOptions{
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

	if _, err := Split(SplitOptions{
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

	if _, err := Split(SplitOptions{
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

	if _, err := Split(SplitOptions{
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

	// Include directory entries (as JARs typically do) to verify they route
	// to artifact layers rather than leaking to the fallback.
	createTestJar(t, jarPath, map[string]string{
		"META-INF/MANIFEST.MF":                 "Manifest-Version: 1.0\n",
		"com/":                                  "",
		"com/google/":                           "",
		"com/google/common/":                    "",
		"com/google/common/collect/":            "",
		"com/google/common/collect/Lists.class": "lists-bytes",
		"com/google/common/base/":               "",
		"com/google/common/base/Strings.class":  "strings-bytes",
		"javax/":                                "",
		"javax/annotation/":                     "",
		"javax/annotation/Nonnull.class":        "nonnull-bytes",
		"example/":                              "",
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

	if _, err := Split(SplitOptions{
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
	// Should also contain ancestor directory entries.
	if _, ok := guavaEntries["com/google/common/collect/"]; !ok {
		t.Error("expected com/google/common/collect/ dir in guava layer")
	}
	if _, ok := guavaEntries["com/google/common/base/"]; !ok {
		t.Error("expected com/google/common/base/ dir in guava layer")
	}

	jsr305Entries := readTar(t, jsr305Path)
	if _, ok := jsr305Entries["javax/annotation/Nonnull.class"]; !ok {
		t.Error("expected Nonnull.class in jsr305 layer")
	}
	if _, ok := jsr305Entries["javax/annotation/"]; !ok {
		t.Error("expected javax/annotation/ dir in jsr305 layer")
	}

	// Fallback should have META-INF, example dir + class.
	// Shared ancestor dirs (com/, javax/) may go to an artifact layer.
	fallbackEntries := readTar(t, fallbackPath)
	if _, ok := fallbackEntries["META-INF/MANIFEST.MF"]; !ok {
		t.Error("expected MANIFEST.MF in fallback")
	}
	if _, ok := fallbackEntries["example/Main.class"]; !ok {
		t.Error("expected Main.class in fallback")
	}
	// Verify no artifact files leaked to fallback.
	for name := range fallbackEntries {
		if strings.HasPrefix(name, "com/google/") || strings.HasPrefix(name, "javax/annotation/") {
			t.Errorf("unexpected artifact entry %s in fallback tar", name)
		}
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
	if _, err := Split(SplitOptions{
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

	if _, err := Split(SplitOptions{
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

func TestExtractMainClass(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		want     string
	}{
		{
			name:     "standard",
			manifest: "Manifest-Version: 1.0\nMain-Class: com.example.Main\n",
			want:     "com.example.Main",
		},
		{
			name:     "with carriage return",
			manifest: "Manifest-Version: 1.0\r\nMain-Class: com.example.Main\r\n",
			want:     "com.example.Main",
		},
		{
			name: "continuation line",
			manifest: "Manifest-Version: 1.0\n" +
				"Main-Class: com.example.very.long.package.name.MainApplicat\n" +
				" ion\n",
			want: "com.example.very.long.package.name.MainApplication",
		},
		{
			name:     "missing main class",
			manifest: "Manifest-Version: 1.0\n",
			want:     "",
		},
		{
			name:     "empty manifest",
			manifest: "",
			want:     "",
		},
		{
			name:     "extra whitespace",
			manifest: "Main-Class:   com.example.Main  \n",
			want:     "com.example.Main",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractMainClass(tt.manifest)
			if got != tt.want {
				t.Errorf("extractMainClass() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSplit_EntrypointGeneration(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "test.jar")
	fallbackPath := filepath.Join(dir, "fallback.tar")
	entrypointPath := filepath.Join(dir, "entrypoint.sh")

	createTestJar(t, jarPath, map[string]string{
		"META-INF/MANIFEST.MF":  "Manifest-Version: 1.0\nMain-Class: com.example.Main\n",
		"com/example/Main.class": "main-class-bytes",
	})

	result, err := Split(SplitOptions{
		InputPath:      jarPath,
		FallbackPath:   fallbackPath,
		EntrypointPath: entrypointPath,
		AppPrefix:      "/app",
	})
	if err != nil {
		t.Fatal(err)
	}

	if result.MainClass != "com.example.Main" {
		t.Errorf("MainClass = %q, want %q", result.MainClass, "com.example.Main")
	}

	// Verify entrypoint file content.
	data, err := os.ReadFile(entrypointPath)
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	wantScript := "#!/bin/sh\nexec java ${JAVA_OPTS} -cp /app com.example.Main \"$@\"\n"
	if script != wantScript {
		t.Errorf("entrypoint script:\ngot:  %q\nwant: %q", script, wantScript)
	}

	// Verify file is executable.
	info, err := os.Stat(entrypointPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0111 == 0 {
		t.Errorf("entrypoint should be executable, got mode %v", info.Mode().Perm())
	}
}

func TestSplit_EntrypointNoManifest(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "test.jar")
	fallbackPath := filepath.Join(dir, "fallback.tar")
	entrypointPath := filepath.Join(dir, "entrypoint.sh")

	createTestJar(t, jarPath, map[string]string{
		"com/example/Main.class": "main-class-bytes",
	})

	_, err := Split(SplitOptions{
		InputPath:      jarPath,
		FallbackPath:   fallbackPath,
		EntrypointPath: entrypointPath,
	})
	if err == nil {
		t.Fatal("expected error when no MANIFEST.MF and entrypoint requested")
	}
	if !strings.Contains(err.Error(), "no Main-Class") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSplit_PathPrefix(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "test.jar")
	fallbackPath := filepath.Join(dir, "fallback.tar")

	createTestJar(t, jarPath, map[string]string{
		"META-INF/MANIFEST.MF":  "Manifest-Version: 1.0\n",
		"com/example/Main.class": "main-class-bytes",
	})

	if _, err := Split(SplitOptions{
		InputPath:    jarPath,
		FallbackPath: fallbackPath,
		PathPrefix:   "app/",
	}); err != nil {
		t.Fatal(err)
	}

	entries := readTar(t, fallbackPath)
	if _, ok := entries["app/META-INF/MANIFEST.MF"]; !ok {
		t.Errorf("expected app/META-INF/MANIFEST.MF, got keys: %v", keys(entries))
	}
	if _, ok := entries["app/com/example/Main.class"]; !ok {
		t.Errorf("expected app/com/example/Main.class, got keys: %v", keys(entries))
	}
}

func TestSplit_GroupedArtifacts(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "test.jar")
	fallbackPath := filepath.Join(dir, "fallback.tar")
	groupPath := filepath.Join(dir, "google_group.tar")
	lockFilePath := filepath.Join(dir, "lock.json")

	createTestJar(t, jarPath, map[string]string{
		"META-INF/MANIFEST.MF":                         "Manifest-Version: 1.0\n",
		"com/google/common/collect/Lists.class":         "lists-bytes",
		"com/google/common/util/concurrent/internal/InternalFutureFailureAccess.class": "fa-bytes",
		"javax/annotation/Nonnull.class":                "nonnull-bytes",
		"example/Main.class":                            "main-bytes",
	})

	createLockFile(t, lockFilePath, map[string][]string{
		"com.google.guava:guava": {
			"com.google.common.collect",
		},
		"com.google.guava:failureaccess": {
			"com.google.common.util.concurrent.internal",
		},
		"com.google.code.findbugs:jsr305": {
			"javax.annotation",
		},
	})

	// Group guava + failureaccess + jsr305 all into one tar (simulating group_by_prefix).
	// Multiple artifact IDs share the same OutputPath.
	if _, err := Split(SplitOptions{
		InputPath:         jarPath,
		FallbackPath:      fallbackPath,
		MavenLockFilePath: lockFilePath,
		Artifacts: []Artifact{
			{ID: "com.google.guava:guava", OutputPath: groupPath},
			{ID: "com.google.guava:failureaccess", OutputPath: groupPath},
			{ID: "com.google.code.findbugs:jsr305", OutputPath: groupPath},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// All three artifacts' classes should be in the shared group tar.
	groupEntries := readTar(t, groupPath)
	if _, ok := groupEntries["com/google/common/collect/Lists.class"]; !ok {
		t.Error("expected Lists.class in group layer")
	}
	if _, ok := groupEntries["com/google/common/util/concurrent/internal/InternalFutureFailureAccess.class"]; !ok {
		t.Error("expected InternalFutureFailureAccess.class in group layer")
	}
	if _, ok := groupEntries["javax/annotation/Nonnull.class"]; !ok {
		t.Error("expected Nonnull.class in group layer")
	}
	if len(groupEntries) != 3 {
		t.Errorf("group layer: got %d entries, want 3: %v", len(groupEntries), keys(groupEntries))
	}

	// Fallback should have only META-INF and example.
	fallbackEntries := readTar(t, fallbackPath)
	if _, ok := fallbackEntries["META-INF/MANIFEST.MF"]; !ok {
		t.Error("expected MANIFEST.MF in fallback")
	}
	if _, ok := fallbackEntries["example/Main.class"]; !ok {
		t.Error("expected Main.class in fallback")
	}
	if len(fallbackEntries) != 2 {
		t.Errorf("fallback: got %d entries, want 2: %v", len(fallbackEntries), keys(fallbackEntries))
	}
}

// readTarHeaders reads a tar file and returns a map of entry name -> tar header.
func readTarHeaders(t *testing.T, path string) map[string]*tar.Header {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	result := make(map[string]*tar.Header)
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		result[hdr.Name] = hdr
	}
	return result
}

func TestSplit_DirectoryPermissions(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "test.jar")
	fallbackPath := filepath.Join(dir, "fallback.tar")

	// ZIP directory entries typically have mode bits that include os.ModeDir
	// but zero permission bits. The splitter must ensure directories get 0755.
	createTestJar(t, jarPath, map[string]string{
		"META-INF/":             "",
		"META-INF/MANIFEST.MF":  "Manifest-Version: 1.0\n",
		"com/":                   "",
		"com/example/":           "",
		"com/example/Main.class": "main-class-bytes",
	})

	if _, err := Split(SplitOptions{
		InputPath:    jarPath,
		FallbackPath: fallbackPath,
	}); err != nil {
		t.Fatal(err)
	}

	headers := readTarHeaders(t, fallbackPath)

	// Verify directory entries have execute bit set.
	for _, dirName := range []string{"META-INF/", "com/", "com/example/"} {
		hdr, ok := headers[dirName]
		if !ok {
			t.Errorf("missing directory entry %s", dirName)
			continue
		}
		mode := os.FileMode(hdr.Mode)
		if mode&0111 == 0 {
			t.Errorf("directory %s has no execute bit: %04o", dirName, mode)
		}
	}

	// Verify regular file entries preserve their permissions from the ZIP.
	for _, fileName := range []string{"META-INF/MANIFEST.MF", "com/example/Main.class"} {
		hdr, ok := headers[fileName]
		if !ok {
			t.Errorf("missing file entry %s", fileName)
			continue
		}
		mode := os.FileMode(hdr.Mode)
		if mode&0400 == 0 {
			t.Errorf("file %s is not readable: %04o", fileName, mode)
		}
	}
}

func TestSplit_DirectoryPermissionsWithPathPrefix(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "test.jar")
	fallbackPath := filepath.Join(dir, "fallback.tar")

	createTestJar(t, jarPath, map[string]string{
		"com/":                   "",
		"com/example/":           "",
		"com/example/Main.class": "main-class-bytes",
	})

	if _, err := Split(SplitOptions{
		InputPath:    jarPath,
		FallbackPath: fallbackPath,
		PathPrefix:   "app/",
	}); err != nil {
		t.Fatal(err)
	}

	headers := readTarHeaders(t, fallbackPath)

	for _, dirName := range []string{"app/com/", "app/com/example/"} {
		hdr, ok := headers[dirName]
		if !ok {
			t.Errorf("missing directory entry %s", dirName)
			continue
		}
		mode := os.FileMode(hdr.Mode)
		if mode&0111 == 0 {
			t.Errorf("directory %s has no execute bit: %04o", dirName, mode)
		}
	}
}

func TestSplit_DirectoryPermissionsInLayers(t *testing.T) {
	dir := t.TempDir()
	jarPath := filepath.Join(dir, "test.jar")
	fallbackPath := filepath.Join(dir, "fallback.tar")
	layerPath := filepath.Join(dir, "layer.tar")

	createTestJar(t, jarPath, map[string]string{
		"com/":                                  "",
		"com/google/":                           "",
		"com/google/common/":                    "",
		"com/google/common/collect/":            "",
		"com/google/common/collect/Lists.class": "lists-bytes",
	})

	if _, err := Split(SplitOptions{
		InputPath:    jarPath,
		FallbackPath: fallbackPath,
		Layers: []Layer{
			{Prefix: "com/google/", OutputPath: layerPath},
		},
	}); err != nil {
		t.Fatal(err)
	}

	headers := readTarHeaders(t, layerPath)

	for _, dirName := range []string{"com/google/", "com/google/common/", "com/google/common/collect/"} {
		hdr, ok := headers[dirName]
		if !ok {
			t.Errorf("missing directory entry %s in layer", dirName)
			continue
		}
		mode := os.FileMode(hdr.Mode)
		if mode&0111 == 0 {
			t.Errorf("directory %s in layer has no execute bit: %04o", dirName, mode)
		}
	}
}

func keys(m map[string]string) []string {
	result := make([]string, 0, len(m))
	for k := range m {
		result = append(result, k)
	}
	return result
}
