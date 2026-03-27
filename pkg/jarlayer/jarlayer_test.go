package jarlayer

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

func TestLayerJars_AllToFallback(t *testing.T) {
	dir := t.TempDir()

	// Create two test JARs.
	jar1 := filepath.Join(dir, "dep1.jar")
	createTestJar(t, jar1, map[string]string{
		"com/example/Foo.class": "foo-bytes",
		"reference.conf":        "dep1-config",
	})
	jar2 := filepath.Join(dir, "dep2.jar")
	createTestJar(t, jar2, map[string]string{
		"org/other/Bar.class": "bar-bytes",
		"reference.conf":      "dep2-config",
	})

	fallbackPath := filepath.Join(dir, "fallback.tar")
	classpathPath := filepath.Join(dir, "classpath")

	err := LayerJars(LayerOptions{
		JarPaths:      []string{jar1, jar2},
		FallbackPath:  fallbackPath,
		ClasspathPath: classpathPath,
		AppPrefix:     "/app/lib",
		PathPrefix:    "app/lib/",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Both JARs should be in fallback (no artifact layers configured).
	entries := readTar(t, fallbackPath)
	if _, ok := entries["app/lib/dep1.jar"]; !ok {
		t.Error("expected dep1.jar in fallback tar")
	}
	if _, ok := entries["app/lib/dep2.jar"]; !ok {
		t.Error("expected dep2.jar in fallback tar")
	}

	// Classpath should list both JARs.
	cpData, err := os.ReadFile(classpathPath)
	if err != nil {
		t.Fatal(err)
	}
	cp := string(cpData)
	if !strings.Contains(cp, "/app/lib/dep1.jar") {
		t.Errorf("classpath missing dep1.jar: %s", cp)
	}
	if !strings.Contains(cp, "/app/lib/dep2.jar") {
		t.Errorf("classpath missing dep2.jar: %s", cp)
	}

	// Classpath file should also be inside the fallback tar.
	if tarCP, ok := entries["app/lib/classpath"]; !ok {
		t.Error("expected classpath file in fallback tar")
	} else if tarCP != cp {
		t.Errorf("classpath in tar differs from file: tar=%q file=%q", tarCP, cp)
	}
}

func TestLayerJars_ArtifactRouting(t *testing.T) {
	dir := t.TempDir()

	// Create JARs with distinct packages.
	guavaJar := filepath.Join(dir, "guava-31.1.jar")
	createTestJar(t, guavaJar, map[string]string{
		"com/google/common/collect/Lists.class": "lists-bytes",
		"com/google/common/base/Strings.class":  "strings-bytes",
	})

	pekkoJar := filepath.Join(dir, "pekko-actor.jar")
	createTestJar(t, pekkoJar, map[string]string{
		"org/apache/pekko/actor/ActorRef.class": "actor-bytes",
	})

	appJar := filepath.Join(dir, "app.jar")
	createTestJar(t, appJar, map[string]string{
		"com/myapp/Main.class": "main-bytes",
		"reference.conf":       "app-config",
	})

	// Lock file maps artifact IDs to packages.
	lockPath := filepath.Join(dir, "lock.json")
	createLockFile(t, lockPath, map[string][]string{
		"com.google.guava:guava":             {"com.google.common.collect", "com.google.common.base"},
		"org.apache.pekko:pekko-actor_2.13":  {"org.apache.pekko.actor"},
	})

	guavaLayerPath := filepath.Join(dir, "guava.tar")
	pekkoLayerPath := filepath.Join(dir, "pekko.tar")
	fallbackPath := filepath.Join(dir, "fallback.tar")
	classpathPath := filepath.Join(dir, "classpath")

	err := LayerJars(LayerOptions{
		JarPaths:     []string{guavaJar, pekkoJar, appJar},
		FallbackPath: fallbackPath,
		LockFilePath: lockPath,
		ArtifactLayers: []ArtifactLayer{
			{IDs: []string{"com.google.guava:guava"}, OutputPath: guavaLayerPath},
			{IDs: []string{"org.apache.pekko:pekko-actor_2.13"}, OutputPath: pekkoLayerPath},
		},
		ClasspathPath: classpathPath,
		AppPrefix:     "/app/lib",
		PathPrefix:    "app/lib/",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Guava JAR should be in guava layer.
	guavaEntries := readTar(t, guavaLayerPath)
	if _, ok := guavaEntries["app/lib/guava-31.1.jar"]; !ok {
		t.Errorf("expected guava-31.1.jar in guava layer, got: %v", keys(guavaEntries))
	}

	// Pekko JAR should be in pekko layer.
	pekkoEntries := readTar(t, pekkoLayerPath)
	if _, ok := pekkoEntries["app/lib/pekko-actor.jar"]; !ok {
		t.Errorf("expected pekko-actor.jar in pekko layer, got: %v", keys(pekkoEntries))
	}

	// App JAR should be in fallback (no matching artifact).
	fallbackEntries := readTar(t, fallbackPath)
	if _, ok := fallbackEntries["app/lib/app.jar"]; !ok {
		t.Errorf("expected app.jar in fallback, got: %v", keys(fallbackEntries))
	}
}

func TestLayerJars_GroupedArtifacts(t *testing.T) {
	dir := t.TempDir()

	guavaJar := filepath.Join(dir, "guava.jar")
	createTestJar(t, guavaJar, map[string]string{
		"com/google/common/collect/Lists.class": "lists",
	})

	protobufJar := filepath.Join(dir, "protobuf.jar")
	createTestJar(t, protobufJar, map[string]string{
		"com/google/protobuf/Message.class": "msg",
	})

	lockPath := filepath.Join(dir, "lock.json")
	createLockFile(t, lockPath, map[string][]string{
		"com.google.guava:guava":         {"com.google.common.collect"},
		"com.google.protobuf:protobuf-java": {"com.google.protobuf"},
	})

	googleLayerPath := filepath.Join(dir, "google.tar")
	fallbackPath := filepath.Join(dir, "fallback.tar")

	err := LayerJars(LayerOptions{
		JarPaths:     []string{guavaJar, protobufJar},
		FallbackPath: fallbackPath,
		LockFilePath: lockPath,
		ArtifactLayers: []ArtifactLayer{
			{
				IDs:        []string{"com.google.guava:guava", "com.google.protobuf:protobuf-java"},
				OutputPath: googleLayerPath,
			},
		},
		ClasspathPath: "",
		AppPrefix:     "/app/lib",
		PathPrefix:    "app/lib/",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Both JARs should be in the shared google layer.
	entries := readTar(t, googleLayerPath)
	if _, ok := entries["app/lib/guava.jar"]; !ok {
		t.Errorf("expected guava.jar in google layer, got: %v", keys(entries))
	}
	if _, ok := entries["app/lib/protobuf.jar"]; !ok {
		t.Errorf("expected protobuf.jar in google layer, got: %v", keys(entries))
	}

	// Fallback should be empty (no entries besides dirs).
	fallbackEntries := readTar(t, fallbackPath)
	for k := range fallbackEntries {
		if !strings.HasSuffix(k, "/") {
			t.Errorf("unexpected file in fallback: %s", k)
		}
	}
}

func TestLayerJars_JarsPreserveContents(t *testing.T) {
	dir := t.TempDir()

	// Create a JAR with a reference.conf — the key file that must survive.
	jarPath := filepath.Join(dir, "dep.jar")
	createTestJar(t, jarPath, map[string]string{
		"com/example/Svc.class": "svc-bytes",
		"reference.conf":        "important-config-content",
	})

	fallbackPath := filepath.Join(dir, "fallback.tar")

	err := LayerJars(LayerOptions{
		JarPaths:     []string{jarPath},
		FallbackPath: fallbackPath,
		AppPrefix:    "/app/lib",
		PathPrefix:   "app/lib/",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Extract the JAR from the tar and verify its contents are intact.
	entries := readTar(t, fallbackPath)
	jarContent, ok := entries["app/lib/dep.jar"]
	if !ok {
		t.Fatal("dep.jar not found in tar")
	}

	// The JAR content should be a valid ZIP.
	tmpJar := filepath.Join(dir, "extracted.jar")
	if err := os.WriteFile(tmpJar, []byte(jarContent), 0644); err != nil {
		t.Fatal(err)
	}

	zr, err := zip.OpenReader(tmpJar)
	if err != nil {
		t.Fatal("extracted JAR is not valid ZIP:", err)
	}
	defer zr.Close()

	foundRefConf := false
	for _, f := range zr.File {
		if f.Name == "reference.conf" {
			rc, _ := f.Open()
			data, _ := io.ReadAll(rc)
			rc.Close()
			if string(data) != "important-config-content" {
				t.Errorf("reference.conf content mismatch: got %q", string(data))
			}
			foundRefConf = true
		}
	}
	if !foundRefConf {
		t.Error("reference.conf not found in extracted JAR — this is the bug we're fixing!")
	}
}

func TestUniqueJarName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// External maven deps: keep basename.
		{
			input: "bazel-out/darwin_arm64-fastbuild/bin/external/rules_jvm_external~~maven~maven2/com/google/guava/guava/31.1/processed_guava-31.1.jar",
			want:  "processed_guava-31.1.jar",
		},
		{
			input: "external/rules_scala~~scala_deps~io_bazel_rules_scala_scala_library_2_13_17/scala-library-2.13.18.jar",
			want:  "scala-library-2.13.18.jar",
		},
		// Internal workspace JARs: flatten path after /bin/.
		{
			input: "bazel-out/darwin_arm64-fastbuild/bin/trumid/common/aeron/core/scala.jar",
			want:  "trumid_common_aeron_core_scala.jar",
		},
		{
			input: "bazel-out/darwin_arm64-fastbuild/bin/examples/aeron/server/server_lib.jar",
			want:  "examples_aeron_server_server_lib.jar",
		},
		{
			input: "bazel-out/darwin_arm64-fastbuild/bin/trumid/common/aeron/core/macros/scala.jar",
			want:  "trumid_common_aeron_core_macros_scala.jar",
		},
		// Fallback: plain basename.
		{
			input: "some/other/path/foo.jar",
			want:  "foo.jar",
		},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := uniqueJarName(tt.input)
			if got != tt.want {
				t.Errorf("uniqueJarName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestLayerJars_DuplicateBasenames(t *testing.T) {
	dir := t.TempDir()

	// Simulate two internal Scala JARs with the same basename "scala.jar"
	// but different Bazel package paths.
	pkg1 := filepath.Join(dir, "bin", "trumid", "common", "aeron", "core")
	pkg2 := filepath.Join(dir, "bin", "trumid", "common", "bytebuffer")
	os.MkdirAll(pkg1, 0755)
	os.MkdirAll(pkg2, 0755)

	jar1 := filepath.Join(pkg1, "scala.jar")
	createTestJar(t, jar1, map[string]string{
		"com/aeron/Core.class": "core-bytes",
	})
	jar2 := filepath.Join(pkg2, "scala.jar")
	createTestJar(t, jar2, map[string]string{
		"com/bytebuffer/Buf.class": "buf-bytes",
	})

	fallbackPath := filepath.Join(dir, "fallback.tar")
	classpathPath := filepath.Join(dir, "classpath")

	err := LayerJars(LayerOptions{
		JarPaths:      []string{jar1, jar2},
		FallbackPath:  fallbackPath,
		ClasspathPath: classpathPath,
		AppPrefix:     "/app/lib",
		PathPrefix:    "app/lib/",
	})
	if err != nil {
		t.Fatal(err)
	}

	entries := readTar(t, fallbackPath)

	// Both JARs should be present with distinct names derived from their paths.
	var jarEntries []string
	for k := range entries {
		if strings.HasSuffix(k, ".jar") {
			jarEntries = append(jarEntries, k)
		}
	}

	if len(jarEntries) != 2 {
		t.Fatalf("expected 2 JAR entries, got %d: %v", len(jarEntries), jarEntries)
	}

	// Verify the names are different.
	if jarEntries[0] == jarEntries[1] {
		t.Errorf("JAR entries have identical names: %v", jarEntries)
	}

	// Classpath should have 2 distinct entries.
	cpData, err := os.ReadFile(classpathPath)
	if err != nil {
		t.Fatal(err)
	}
	cpEntries := strings.Split(string(cpData), ":")
	if len(cpEntries) != 2 {
		t.Errorf("expected 2 classpath entries, got %d: %s", len(cpEntries), string(cpData))
	}
	if cpEntries[0] == cpEntries[1] {
		t.Errorf("classpath entries are identical: %s", string(cpData))
	}
}

func keys(m map[string]string) []string {
	var result []string
	for k := range m {
		result = append(result, k)
	}
	return result
}
