// Package jarlayer places intact JVM runtime JARs into OCI-compatible tar layers.
package jarlayer

import (
	"archive/tar"
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
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

// DataFile maps an input file or directory to a path in a data layer tar.
type DataFile struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
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
func LayerJars(opts LayerOptions) (retErr error) {
	if err := validateOptions(opts); err != nil {
		return err
	}

	// Parse lock file to build package→artifact_id mapping.
	pkgToArtifact, err := buildPackageMap(opts.LockFilePath)
	if err != nil {
		return err
	}

	// Build artifact_id→tar writer mapping.
	artifactToWriter := make(map[string]*layerWriter)
	writersByPath := make(map[string]*layerWriter)
	var openWriters []*layerWriter
	defer func() {
		var closeErr error
		for i := len(openWriters) - 1; i >= 0; i-- {
			closeErr = errors.Join(closeErr, openWriters[i].Close())
		}
		if closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("closing output tar: %w", closeErr))
		}
	}()

	for _, al := range opts.ArtifactLayers {
		lw, ok := writersByPath[al.OutputPath]
		if !ok {
			lw, err = newLayerWriter(al.OutputPath)
			if err != nil {
				return err
			}
			writersByPath[al.OutputPath] = lw
			openWriters = append(openWriters, lw)
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
	openWriters = append(openWriters, fallback)

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
		if strings.ContainsAny(jarName, "/:\x00\r\n\t ") || jarName == "." || jarName == ".." {
			return fmt.Errorf("JAR %s produces unsupported classpath name %q", jarPath, jarName)
		}
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
		classpathEntries = append(classpathEntries, path.Join(opts.AppPrefix, jarName))
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

// LayerData writes files into a deterministic tar using their Bazel runfiles
// paths. Directories are expanded recursively and symlinks are rejected.
func LayerData(outputPath string, files []DataFile) (retErr error) {
	if outputPath == "" {
		return errors.New("data layer output path is required")
	}

	lw, err := newLayerWriter(outputPath)
	if err != nil {
		return fmt.Errorf("creating data layer: %w", err)
	}
	defer func() {
		if closeErr := lw.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("closing data layer: %w", closeErr))
		}
	}()

	orderedFiles := append([]DataFile(nil), files...)
	sort.Slice(orderedFiles, func(i, j int) bool {
		if orderedFiles[i].Destination == orderedFiles[j].Destination {
			return orderedFiles[i].Source < orderedFiles[j].Source
		}
		return orderedFiles[i].Destination < orderedFiles[j].Destination
	})
	written := make(map[string]string)
	for _, file := range orderedFiles {
		if err := writeDataPath(lw.tw, file.Source, file.Destination, written); err != nil {
			return err
		}
	}
	return nil
}

func writeDataPath(tw *tar.Writer, source, destination string, written map[string]string) error {
	if source == "" {
		return errors.New("data source path is empty")
	}
	if err := validateArchivePath(destination); err != nil {
		return fmt.Errorf("invalid data destination %q: %w", destination, err)
	}

	linkInfo, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("stating data source %s: %w", source, err)
	}
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("resolving data source %s: %w", source, err)
	}
	if info.IsDir() {
		walkRoot := source
		if linkInfo.Mode()&os.ModeSymlink != 0 {
			walkRoot, err = filepath.EvalSymlinks(source)
			if err != nil {
				return fmt.Errorf("resolving data directory %s: %w", source, err)
			}
		}
		return filepath.WalkDir(walkRoot, func(child string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if child == walkRoot {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("data directory %s contains symlink %s", source, child)
			}
			if entry.IsDir() {
				return nil
			}
			relative, err := filepath.Rel(walkRoot, child)
			if err != nil {
				return err
			}
			return writeDataPath(tw, child, path.Join(destination, filepath.ToSlash(relative)), written)
		})
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("data source %s is not a regular file or directory", source)
	}
	if previous, ok := written[destination]; ok {
		if previous == source {
			return nil
		}
		return fmt.Errorf("data sources %s and %s collide at %s", previous, source, destination)
	}
	written[destination] = source

	f, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("opening data source %s: %w", source, err)
	}
	defer f.Close()

	mode := int64(0644)
	if info.Mode().Perm()&0111 != 0 {
		mode = 0755
	}
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     destination,
		Size:     info.Size(),
		Mode:     mode,
	}); err != nil {
		return fmt.Errorf("writing data header %s: %w", destination, err)
	}
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("writing data file %s: %w", destination, err)
	}
	return nil
}

func validateArchivePath(name string) error {
	if name == "" || path.IsAbs(name) {
		return errors.New("path must be non-empty and relative")
	}
	if strings.ContainsRune(name, '\x00') || strings.ContainsAny(name, "\\\r\n") {
		return errors.New("path contains an invalid character")
	}
	if name != path.Clean(name) || hasParentPathSegment(name) {
		return errors.New("path must be clean and must not escape the archive root")
	}
	return nil
}

// buildPackageMap parses the lock file and returns a mapping from Java package
// prefix (e.g., "com/google/common/collect/") to artifact ID. Split packages
// map to the empty string so they cannot cause nondeterministic routing.
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
	artifactIDs := make([]string, 0, len(lf.Packages))
	for artifactID := range lf.Packages {
		artifactIDs = append(artifactIDs, artifactID)
	}
	sort.Strings(artifactIDs)
	for _, artifactID := range artifactIDs {
		packages := lf.Packages[artifactID]
		for _, pkg := range packages {
			prefix := strings.ReplaceAll(pkg, ".", "/") + "/"
			if err := validateArchivePrefix(prefix); err != nil {
				return nil, fmt.Errorf("invalid package %q for artifact %q: %w", pkg, artifactID, err)
			}
			if existing, ok := result[prefix]; ok && existing != artifactID {
				// An empty value marks a split package. It cannot identify a JAR
				// by itself, but another unique package in the JAR still can.
				result[prefix] = ""
				continue
			}
			result[prefix] = artifactID
		}
	}

	return result, nil
}

// identifyArtifact opens a JAR and inspects its .class entries to determine
// which Maven artifact it belongs to via the package→artifact mapping. JARs
// matching multiple artifacts are left unmatched for fallback routing.
func identifyArtifact(jarPath string, pkgToArtifact map[string]string) (string, error) {
	if pkgToArtifact == nil {
		return "", nil
	}

	zr, err := zip.OpenReader(jarPath)
	if err != nil {
		return "", fmt.Errorf("opening jar: %w", err)
	}
	defer zr.Close()

	matches := make(map[string]bool)
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
				if artifactID != "" {
					matches[artifactID] = true
				}
				break
			}
			// Try parent: "com/google/common/collect/" → "com/google/common/"
			trimmed := strings.TrimSuffix(pkg, "/")
			lastSlash := strings.LastIndex(trimmed, "/")
			if lastSlash < 0 {
				break
			}
			pkg = trimmed[:lastSlash+1]
		}

	}

	if len(matches) == 0 {
		return "", nil
	}
	if len(matches) > 1 {
		return "", nil
	}
	for artifactID := range matches {
		return artifactID, nil
	}
	return "", errors.New("internal error resolving artifact match")
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
	tarErr := lw.tw.Close()
	fileErr := lw.file.Close()
	return errors.Join(tarErr, fileErr)
}

func validateOptions(opts LayerOptions) error {
	if opts.FallbackPath == "" {
		return errors.New("fallback output path is required")
	}
	if err := validateArchivePrefix(opts.PathPrefix); err != nil {
		return fmt.Errorf("invalid path prefix: %w", err)
	}
	if opts.AppPrefix == "" || !path.IsAbs(opts.AppPrefix) {
		return errors.New("app prefix must be an absolute container path")
	}
	if strings.ContainsAny(opts.AppPrefix, ":\\\x00\r\n\t ") || hasParentPathSegment(opts.AppPrefix) {
		return errors.New("app prefix contains a classpath separator or invalid character")
	}

	outputs := map[string]string{opts.FallbackPath: "fallback output"}
	if opts.ClasspathPath != "" {
		outputs[opts.ClasspathPath] = "classpath output"
		if opts.ClasspathPath == opts.FallbackPath {
			return fmt.Errorf("output path %q is shared by fallback and classpath outputs", opts.FallbackPath)
		}
	}
	artifactIDs := make(map[string]string)
	for i, layer := range opts.ArtifactLayers {
		if layer.OutputPath == "" {
			return fmt.Errorf("artifact layer %d has an empty output path", i)
		}
		if len(layer.IDs) == 0 {
			return fmt.Errorf("artifact layer %d has no artifact IDs", i)
		}
		if owner, ok := outputs[layer.OutputPath]; ok {
			return fmt.Errorf("output path %q is shared by %s and an artifact layer", layer.OutputPath, owner)
		}
		for _, id := range layer.IDs {
			if id == "" {
				return fmt.Errorf("artifact layer %d has an empty artifact ID", i)
			}
			if outputPath, ok := artifactIDs[id]; ok && outputPath != layer.OutputPath {
				return fmt.Errorf("artifact %q is assigned to multiple outputs (%s and %s)", id, outputPath, layer.OutputPath)
			}
			artifactIDs[id] = layer.OutputPath
		}
	}
	if len(opts.ArtifactLayers) > 0 && opts.LockFilePath == "" {
		return errors.New("maven lock file is required when artifact layers are configured")
	}
	return nil
}

func validateArchivePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if strings.ContainsRune(prefix, '\x00') || strings.ContainsAny(prefix, "\\\r\n") {
		return errors.New("path contains an invalid character")
	}
	if path.IsAbs(prefix) {
		return errors.New("path must be relative")
	}
	if !strings.HasSuffix(prefix, "/") {
		return errors.New("non-empty prefix must end with a slash")
	}
	cleaned := path.Clean(prefix)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return errors.New("path must not escape the archive root")
	}
	return nil
}

func hasParentPathSegment(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return true
		}
	}
	return false
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
