package jarlayer

import (
	"archive/tar"
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// LayerOptions configures how individual JARs are placed into layered tars.
type LayerOptions struct {
	// JarPaths lists paths to individual JAR files to layer.
	JarPaths []string
	// FallbackPath is the output tar for JARs not matching any artifact layer.
	FallbackPath string
	// ArtifactLayers maps artifact IDs to output tar paths.
	// Multiple artifact IDs may share the same output path (grouped).
	ArtifactLayers []ArtifactLayer
	// LockFilePath is the maven lock file JSON for package→artifact resolution.
	LockFilePath string
	// ClasspathPath is the output path for the classpath file.
	ClasspathPath string
	// AppPrefix is the classpath prefix in the container (e.g., "/app/lib").
	AppPrefix string
	// PathPrefix is prepended to tar entry paths (e.g., "app/lib/").
	PathPrefix string
}

// ArtifactLayer maps one or more artifact IDs to a single output tar.
type ArtifactLayer struct {
	IDs        []string
	OutputPath string
}

// MavenLockFile represents the relevant parts of the maven lock file JSON.
type MavenLockFile struct {
	Packages map[string][]string `json:"packages"`
}

// LayerJars distributes individual JAR files across tar layers based on
// Maven artifact identity, then writes a classpath file listing all JARs
// with their container paths.
//
// For each JAR, the tool inspects its ZIP entries to find a .class file,
// derives the package name, and matches it against the lock file to determine
// which artifact (and thus which layer) the JAR belongs to.
func LayerJars(opts LayerOptions) error {
	// Parse lock file to build package→artifact_id mapping.
	pkgToArtifact, err := buildPackageMap(opts.LockFilePath)
	if err != nil {
		return err
	}

	// Build artifact_id→tar writer mapping.
	artifactToWriter := make(map[string]*layerWriter)
	writersByPath := make(map[string]*layerWriter)

	for _, al := range opts.ArtifactLayers {
		lw, ok := writersByPath[al.OutputPath]
		if !ok {
			lw, err = newLayerWriter(al.OutputPath)
			if err != nil {
				return err
			}
			defer lw.Close()
			writersByPath[al.OutputPath] = lw
		}
		for _, id := range al.IDs {
			artifactToWriter[id] = lw
		}
	}

	// Open fallback tar writer.
	fallback, err := newLayerWriter(opts.FallbackPath)
	if err != nil {
		return fmt.Errorf("creating fallback tar: %w", err)
	}
	defer fallback.Close()

	// Track written directories to avoid duplicates across JARs.
	writtenDirs := make(map[string]map[string]bool) // writer path -> set of dirs

	// Process each JAR: determine layer, write to tar, collect classpath.
	var classpathEntries []string
	usedNames := make(map[string]bool)

	for _, jarPath := range opts.JarPaths {
		// Determine which artifact this JAR belongs to.
		artifactID, err := identifyArtifact(jarPath, pkgToArtifact)
		if err != nil {
			return fmt.Errorf("identifying artifact for %s: %w", jarPath, err)
		}

		// Select the target writer.
		lw := fallback
		if artifactID != "" {
			if w, ok := artifactToWriter[artifactID]; ok {
				lw = w
			}
		}

		// Determine a unique tar entry name from the Bazel path.
		jarName := uniqueJarName(jarPath)
		if usedNames[jarName] {
			// Collision fallback: append numeric suffix.
			ext := path.Ext(jarName)
			base := strings.TrimSuffix(jarName, ext)
			for i := 2; ; i++ {
				candidate := fmt.Sprintf("%s_%d%s", base, i, ext)
				if !usedNames[candidate] {
					jarName = candidate
					break
				}
			}
		}
		usedNames[jarName] = true
		entryPath := opts.PathPrefix + jarName

		// Ensure parent directory exists in the tar.
		if opts.PathPrefix != "" {
			if err := ensureParentDirs(lw, opts.PathPrefix, writtenDirs); err != nil {
				return err
			}
		}

		// Write the JAR file as-is to the tar.
		if err := writeJarToTar(lw.tw, jarPath, entryPath); err != nil {
			return fmt.Errorf("writing %s to tar: %w", jarPath, err)
		}

		// Record classpath entry.
		classpathEntries = append(classpathEntries, opts.AppPrefix+"/"+jarName)
	}

	// Write classpath file.
	if opts.ClasspathPath != "" {
		classpath := strings.Join(classpathEntries, ":")
		if err := os.WriteFile(opts.ClasspathPath, []byte(classpath), 0644); err != nil {
			return fmt.Errorf("writing classpath file: %w", err)
		}

		// Also write the classpath file into the fallback tar so it
		// appears in the container filesystem at <pathPrefix>classpath.
		classpathEntry := opts.PathPrefix + "classpath"
		classpathBytes := []byte(classpath)
		if err := fallback.tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     classpathEntry,
			Size:     int64(len(classpathBytes)),
			Mode:     0644,
		}); err != nil {
			return fmt.Errorf("writing classpath tar entry: %w", err)
		}
		if _, err := fallback.tw.Write(classpathBytes); err != nil {
			return fmt.Errorf("writing classpath tar data: %w", err)
		}
	}

	return nil
}

// buildPackageMap parses the lock file and returns a mapping from
// Java package prefix (e.g., "com/google/common/collect/") to artifact ID.
func buildPackageMap(lockFilePath string) (map[string]string, error) {
	if lockFilePath == "" {
		return nil, nil
	}

	data, err := os.ReadFile(lockFilePath)
	if err != nil {
		return nil, fmt.Errorf("reading lock file: %w", err)
	}

	var lf MavenLockFile
	if err := json.Unmarshal(data, &lf); err != nil {
		return nil, fmt.Errorf("parsing lock file: %w", err)
	}

	result := make(map[string]string)
	for artifactID, packages := range lf.Packages {
		for _, pkg := range packages {
			prefix := strings.ReplaceAll(pkg, ".", "/") + "/"
			result[prefix] = artifactID
		}
	}

	return result, nil
}

// identifyArtifact opens a JAR and finds the first .class entry to determine
// which Maven artifact it belongs to via the package→artifact mapping.
func identifyArtifact(jarPath string, pkgToArtifact map[string]string) (string, error) {
	if pkgToArtifact == nil {
		return "", nil
	}

	zr, err := zip.OpenReader(jarPath)
	if err != nil {
		return "", fmt.Errorf("opening jar: %w", err)
	}
	defer zr.Close()

	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".class") {
			continue
		}
		// Derive package prefix from class path.
		// e.g., "com/google/common/collect/Lists.class" → "com/google/common/collect/"
		idx := strings.LastIndex(f.Name, "/")
		if idx < 0 {
			continue // default package, skip
		}
		pkg := f.Name[:idx+1]

		// Walk up the package hierarchy to find a match.
		for pkg != "" {
			if artifactID, ok := pkgToArtifact[pkg]; ok {
				return artifactID, nil
			}
			// Try parent: "com/google/common/collect/" → "com/google/common/"
			trimmed := strings.TrimSuffix(pkg, "/")
			lastSlash := strings.LastIndex(trimmed, "/")
			if lastSlash < 0 {
				break
			}
			pkg = trimmed[:lastSlash+1]
		}

		// First class entry didn't match; keep trying other entries.
	}

	return "", nil // no match found
}

type layerWriter struct {
	path string
	file *os.File
	tw   *tar.Writer
}

func newLayerWriter(outputPath string) (*layerWriter, error) {
	f, err := os.Create(outputPath)
	if err != nil {
		return nil, fmt.Errorf("creating %s: %w", outputPath, err)
	}
	return &layerWriter{
		path: outputPath,
		file: f,
		tw:   tar.NewWriter(f),
	}, nil
}

func (lw *layerWriter) Close() error {
	if err := lw.tw.Close(); err != nil {
		lw.file.Close()
		return err
	}
	return lw.file.Close()
}

// ensureParentDirs writes directory entries for all path components of prefix
// that haven't been written yet to this writer.
func ensureParentDirs(lw *layerWriter, prefix string, writtenDirs map[string]map[string]bool) error {
	dirs, ok := writtenDirs[lw.path]
	if !ok {
		dirs = make(map[string]bool)
		writtenDirs[lw.path] = dirs
	}

	// Build directory components: "app/lib/" → ["app/", "app/lib/"]
	parts := strings.Split(strings.TrimSuffix(prefix, "/"), "/")
	current := ""
	for _, part := range parts {
		current += part + "/"
		if dirs[current] {
			continue
		}
		dirs[current] = true
		if err := lw.tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir,
			Name:     current,
			Mode:     0755,
		}); err != nil {
			return fmt.Errorf("writing dir %s: %w", current, err)
		}
	}
	return nil
}

// uniqueJarName derives a unique filename for a JAR based on its Bazel path.
//
// External maven JARs (paths containing "/external/" or starting with "external/")
// already have unique basenames like "processed_guava-31.1.jar".
//
// Internal workspace JARs (paths containing "/bin/") use the package-relative
// path with slashes replaced by underscores, e.g.:
//
//	"bazel-out/.../bin/trumid/common/aeron/core/scala.jar" → "trumid_common_aeron_core_scala.jar"
func uniqueJarName(jarPath string) string {
	// External maven deps: basename is already unique.
	if i := strings.Index(jarPath, "/external/"); i >= 0 {
		return path.Base(jarPath)
	}
	if strings.HasPrefix(jarPath, "external/") {
		return path.Base(jarPath)
	}

	// Internal workspace JARs: use path after /bin/ with slashes→underscores.
	if i := strings.Index(jarPath, "/bin/"); i >= 0 {
		rel := jarPath[i+len("/bin/"):]
		return strings.ReplaceAll(rel, "/", "_")
	}

	// Fallback: just the basename.
	return path.Base(jarPath)
}

// writeJarToTar writes a JAR file as a single tar entry.
func writeJarToTar(tw *tar.Writer, jarPath, entryPath string) error {
	f, err := os.Open(jarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return err
	}

	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     entryPath,
		Size:     info.Size(),
		Mode:     0644,
	}); err != nil {
		return err
	}

	_, err = io.Copy(tw, f)
	return err
}
