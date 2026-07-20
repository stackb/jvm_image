// Package jartar splits an executable JAR into OCI-compatible tar layers.
package jartar

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"sort"
	"strings"
)

// Layer defines an output layer with a path prefix and output file path.
type Layer struct {
	Prefix     string
	OutputPath string
}

// Artifact defines an artifact-based layer with its output path.
// Multiple artifacts may share the same OutputPath when grouped.
type Artifact struct {
	ID         string // e.g. "com.google.guava:guava"
	OutputPath string
}

// MavenLockFile represents the relevant parts of the maven lock file JSON.
type MavenLockFile struct {
	Packages map[string][]string `json:"packages"`
}

// SplitResult contains metadata extracted during the split operation.
type SplitResult struct {
	MainClass string // e.g. "com.example.Main"
}

// SplitOptions configures how a JAR is split into layered tars.
type SplitOptions struct {
	InputPath         string
	FallbackPath      string
	Layers            []Layer
	MavenLockFilePath string
	Artifacts         []Artifact
	PathPrefix        string // prefix prepended to tar entry paths, e.g. "app/"
	EntrypointPath    string // path to write entrypoint shell script (optional)
	AppPrefix         string // classpath prefix in container, e.g. "/app"
}

// Split reads a JAR file and distributes entries across layer tars.
// Routing priority: explicit layers first, then artifact-derived prefixes, then fallback.
// All output tars are always written, even if empty.
func Split(opts SplitOptions) (result *SplitResult, retErr error) {
	if err := validateOptions(opts); err != nil {
		return nil, err
	}

	zr, err := zip.OpenReader(opts.InputPath)
	if err != nil {
		return nil, fmt.Errorf("opening jar: %w", err)
	}
	defer zr.Close()

	// Extract Main-Class from manifest.
	mainClass, err := parseMainClass(zr)
	if err != nil {
		return nil, err
	}

	// Build artifact prefix map if lock file is provided.
	var artifactRoutes []artifactRoute
	var openWriters []*writerState
	defer func() {
		var closeErr error
		for i := len(openWriters) - 1; i >= 0; i-- {
			closeErr = errors.Join(closeErr, openWriters[i].Close())
		}
		if closeErr != nil {
			result = nil
			retErr = errors.Join(retErr, fmt.Errorf("closing output tar: %w", closeErr))
		}
	}()

	if opts.MavenLockFilePath != "" && len(opts.Artifacts) > 0 {
		lockFile, err := parseLockFile(opts.MavenLockFilePath)
		if err != nil {
			return nil, err
		}

		// Deduplicate writers by output path so grouped artifacts share one tar.
		writersByPath := make(map[string]*writerState)
		routesByPrefix := make(map[string]artifactRoute)

		for _, a := range opts.Artifacts {
			lw, ok := writersByPath[a.OutputPath]
			if !ok {
				lw, err = newWriterState(a.OutputPath)
				if err != nil {
					return nil, fmt.Errorf("creating artifact output %s: %w", a.OutputPath, err)
				}
				writersByPath[a.OutputPath] = lw
				openWriters = append(openWriters, lw)
			}

			// Map each package prefix for this artifact to the shared writer.
			for _, pkg := range lockFile.Packages[a.ID] {
				prefix := strings.ReplaceAll(pkg, ".", "/") + "/"
				if err := validateArchivePath(prefix, true); err != nil {
					return nil, fmt.Errorf("invalid package %q for artifact %q: %w", pkg, a.ID, err)
				}
				route := artifactRoute{prefix: prefix, outputPath: a.OutputPath, tw: lw.tw}
				if existing, ok := routesByPrefix[prefix]; ok {
					if existing.outputPath == "" || existing.outputPath == route.outputPath {
						continue
					}
					// Split packages cannot be attributed safely after a deploy JAR
					// has been merged. Keep them in the fallback layer.
					routesByPrefix[prefix] = artifactRoute{prefix: prefix}
					continue
				}
				routesByPrefix[prefix] = route
			}
		}
		for _, route := range routesByPrefix {
			artifactRoutes = append(artifactRoutes, route)
		}
		sort.Slice(artifactRoutes, func(i, j int) bool {
			if len(artifactRoutes[i].prefix) != len(artifactRoutes[j].prefix) {
				return len(artifactRoutes[i].prefix) > len(artifactRoutes[j].prefix)
			}
			return artifactRoutes[i].prefix < artifactRoutes[j].prefix
		})
	}

	// Open explicit layer tar writers.
	layerWriters := make([]*writerState, len(opts.Layers))
	for i, l := range opts.Layers {
		lw, err := newWriterState(l.OutputPath)
		if err != nil {
			return nil, fmt.Errorf("creating layer output %s: %w", l.OutputPath, err)
		}
		openWriters = append(openWriters, lw)
		layerWriters[i] = lw
	}

	// Open fallback tar writer.
	fallback, err := newWriterState(opts.FallbackPath)
	if err != nil {
		return nil, fmt.Errorf("creating fallback output: %w", err)
	}
	openWriters = append(openWriters, fallback)

	for _, f := range zr.File {
		tw := resolveWriter(f.Name, opts.Layers, layerWriters, artifactRoutes, fallback.tw)
		if err := writeEntry(tw, f, opts.PathPrefix); err != nil {
			return nil, fmt.Errorf("writing entry %s: %w", f.Name, err)
		}
	}

	// Generate entrypoint script if requested.
	if opts.EntrypointPath != "" {
		if mainClass == "" {
			return nil, fmt.Errorf("no Main-Class found in MANIFEST.MF; cannot generate entrypoint")
		}
		appPrefix := opts.AppPrefix
		if appPrefix == "" {
			appPrefix = "/app"
		}
		if err := writeEntrypoint(opts.EntrypointPath, appPrefix, mainClass); err != nil {
			return nil, fmt.Errorf("writing entrypoint: %w", err)
		}
	}

	return &SplitResult{MainClass: mainClass}, nil
}

// resolveWriter determines which tar writer should receive the given entry.
// Priority: explicit layers first, then artifact-derived prefixes, then fallback.
func resolveWriter(
	name string,
	layers []Layer,
	layerWriters []*writerState,
	artifactRoutes []artifactRoute,
	fallback *tar.Writer,
) *tar.Writer {
	// Check explicit layers first.
	for i, l := range layers {
		if strings.HasPrefix(name, l.Prefix) {
			return layerWriters[i].tw
		}
	}

	// Check artifact-derived prefixes.
	// Also check if the entry is an ancestor directory of a prefix
	// (e.g. entry "com/google/" is ancestor of prefix "com/google/common/collect/").
	for _, route := range artifactRoutes {
		if strings.HasPrefix(name, route.prefix) || strings.HasPrefix(route.prefix, name) {
			if route.tw == nil {
				return fallback
			}
			return route.tw
		}
	}

	return fallback
}

type writerState struct {
	file *os.File
	tw   *tar.Writer
}

type artifactRoute struct {
	prefix     string
	outputPath string
	tw         *tar.Writer
}

func newWriterState(outputPath string) (*writerState, error) {
	f, err := os.Create(outputPath)
	if err != nil {
		return nil, err
	}
	return &writerState{file: f, tw: tar.NewWriter(f)}, nil
}

func (w *writerState) Close() error {
	tarErr := w.tw.Close()
	fileErr := w.file.Close()
	return errors.Join(tarErr, fileErr)
}

func validateOptions(opts SplitOptions) error {
	if opts.InputPath == "" {
		return errors.New("input path is required")
	}
	if opts.FallbackPath == "" {
		return errors.New("fallback output path is required")
	}
	if err := validateArchivePath(opts.PathPrefix, true); err != nil {
		return fmt.Errorf("invalid path prefix: %w", err)
	}
	if opts.EntrypointPath != "" && opts.AppPrefix != "" {
		if !path.IsAbs(opts.AppPrefix) {
			return errors.New("app prefix must be an absolute container path")
		}
		if strings.ContainsAny(opts.AppPrefix, ":\\\x00\r\n\t ") || hasParentPathSegment(opts.AppPrefix) {
			return errors.New("app prefix contains an invalid character")
		}
	}

	outputs := map[string]string{opts.FallbackPath: "fallback output"}
	for i, layer := range opts.Layers {
		if layer.Prefix == "" {
			return fmt.Errorf("layer %d has an empty prefix", i)
		}
		if err := validateArchivePath(layer.Prefix, false); err != nil {
			return fmt.Errorf("invalid layer prefix %q: %w", layer.Prefix, err)
		}
		if layer.OutputPath == "" {
			return fmt.Errorf("layer %q has an empty output path", layer.Prefix)
		}
		if owner, ok := outputs[layer.OutputPath]; ok {
			return fmt.Errorf("output path %q is shared by %s and layer %q", layer.OutputPath, owner, layer.Prefix)
		}
		outputs[layer.OutputPath] = fmt.Sprintf("layer %q", layer.Prefix)
	}

	artifactIDs := make(map[string]string)
	for i, artifact := range opts.Artifacts {
		if artifact.ID == "" {
			return fmt.Errorf("artifact %d has an empty ID", i)
		}
		if artifact.OutputPath == "" {
			return fmt.Errorf("artifact %q has an empty output path", artifact.ID)
		}
		if owner, ok := outputs[artifact.OutputPath]; ok {
			return fmt.Errorf("output path %q is shared by %s and artifact %q", artifact.OutputPath, owner, artifact.ID)
		}
		if outputPath, ok := artifactIDs[artifact.ID]; ok && outputPath != artifact.OutputPath {
			return fmt.Errorf("artifact %q is assigned to multiple outputs (%s and %s)", artifact.ID, outputPath, artifact.OutputPath)
		}
		artifactIDs[artifact.ID] = artifact.OutputPath
	}
	if len(opts.Artifacts) > 0 && opts.MavenLockFilePath == "" {
		return errors.New("maven lock file is required when artifact layers are configured")
	}
	return nil
}

func validateArchivePath(name string, allowEmpty bool) error {
	if name == "" {
		if allowEmpty {
			return nil
		}
		return errors.New("path must not be empty")
	}
	if strings.ContainsRune(name, '\x00') || strings.ContainsAny(name, "\\\r\n") {
		return errors.New("path contains an invalid character")
	}
	if path.IsAbs(name) {
		return errors.New("path must be relative")
	}
	if allowEmpty && !strings.HasSuffix(name, "/") {
		return errors.New("non-empty prefix must end with a slash")
	}
	cleaned := path.Clean(name)
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

func parseLockFile(path string) (*MavenLockFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading maven lock file: %w", err)
	}
	var lf MavenLockFile
	if err := json.Unmarshal(data, &lf); err != nil {
		return nil, fmt.Errorf("parsing maven lock file: %w", err)
	}
	return &lf, nil
}

// parseMainClass finds and extracts the Main-Class attribute from the JAR's
// META-INF/MANIFEST.MF. Returns empty string if no manifest or no Main-Class.
func parseMainClass(zr *zip.ReadCloser) (string, error) {
	for _, f := range zr.File {
		if strings.EqualFold(f.Name, "META-INF/MANIFEST.MF") {
			rc, err := f.Open()
			if err != nil {
				return "", fmt.Errorf("opening MANIFEST.MF: %w", err)
			}
			defer rc.Close()
			data, err := io.ReadAll(rc)
			if err != nil {
				return "", fmt.Errorf("reading MANIFEST.MF: %w", err)
			}
			return extractMainClass(string(data)), nil
		}
	}
	return "", nil
}

// extractMainClass parses a MANIFEST.MF body and returns the Main-Class value.
// Handles the MANIFEST.MF continuation line format where lines >72 bytes are
// split with a newline followed by a single leading space.
func extractMainClass(manifest string) string {
	// First, join continuation lines (lines starting with a single space).
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(manifest))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, " ") && len(lines) > 0 {
			lines[len(lines)-1] += line[1:] // append without the leading space
		} else {
			lines = append(lines, line)
		}
	}
	for _, line := range lines {
		// Only attributes in the main section apply to the JAR itself.
		if line == "" || line == "\r" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(name, "Main-Class") {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// writeEntrypoint generates a shell script that runs the exploded JAR
// using java -cp.
func writeEntrypoint(path, appPrefix, mainClass string) error {
	script := fmt.Sprintf("#!/bin/sh\nexec java ${JAVA_OPTS} -cp %s %s \"$@\"\n", shellQuote(appPrefix), shellQuote(mainClass))
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		return err
	}
	return os.Chmod(path, 0755)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func writeEntry(tw *tar.Writer, f *zip.File, pathPrefix string) error {
	if err := validateArchivePath(f.Name, false); err != nil {
		return fmt.Errorf("invalid archive path: %w", err)
	}
	if f.UncompressedSize64 > math.MaxInt64 {
		return fmt.Errorf("entry is too large: %d bytes", f.UncompressedSize64)
	}
	info := f.FileInfo()
	isDir := info.IsDir() || strings.HasSuffix(f.Name, "/")

	perm := info.Mode().Perm()
	if isDir {
		// Ensure directories always have execute bits set.
		// ZIP entries from jar tools often have zero or read-only permissions
		// for directories, which prevents traversal when extracted.
		if perm == 0 {
			perm = 0755
		} else {
			perm |= 0111
		}
	} else if perm == 0 {
		perm = 0644
	}

	hdr := &tar.Header{
		Name:    pathPrefix + f.Name,
		ModTime: f.Modified,
		Mode:    int64(perm),
	}

	if isDir {
		hdr.Typeflag = tar.TypeDir
	} else {
		hdr.Typeflag = tar.TypeReg
		hdr.Size = int64(f.UncompressedSize64)
	}

	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}

	if !isDir {
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()

		if _, err := io.Copy(tw, rc); err != nil {
			return err
		}
	}

	return nil
}
