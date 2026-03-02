package jartar

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
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
func Split(opts SplitOptions) (*SplitResult, error) {
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
	var artifactPrefixMap map[string]*tar.Writer // path prefix -> tar writer

	if opts.MavenLockFilePath != "" && len(opts.Artifacts) > 0 {
		lockFile, err := parseLockFile(opts.MavenLockFilePath)
		if err != nil {
			return nil, err
		}

		artifactPrefixMap = make(map[string]*tar.Writer)

		// Deduplicate writers by output path so grouped artifacts share one tar.
		writersByPath := make(map[string]*tar.Writer)

		for _, a := range opts.Artifacts {
			tw, ok := writersByPath[a.OutputPath]
			if !ok {
				f, err := os.Create(a.OutputPath)
				if err != nil {
					return nil, fmt.Errorf("creating artifact output %s: %w", a.OutputPath, err)
				}
				defer f.Close()
				tw = tar.NewWriter(f)
				defer tw.Close()
				writersByPath[a.OutputPath] = tw
			}

			// Map each package prefix for this artifact to the shared writer.
			for _, pkg := range lockFile.Packages[a.ID] {
				prefix := strings.ReplaceAll(pkg, ".", "/") + "/"
				artifactPrefixMap[prefix] = tw
			}
		}
	}

	// Open explicit layer tar writers.
	layerWriters := make([]writerState, len(opts.Layers))
	for i, l := range opts.Layers {
		f, err := os.Create(l.OutputPath)
		if err != nil {
			return nil, fmt.Errorf("creating layer output %s: %w", l.OutputPath, err)
		}
		defer f.Close()
		tw := tar.NewWriter(f)
		defer tw.Close()
		layerWriters[i] = writerState{file: f, tw: tw}
	}

	// Open fallback tar writer.
	fallbackFile, err := os.Create(opts.FallbackPath)
	if err != nil {
		return nil, fmt.Errorf("creating fallback output: %w", err)
	}
	defer fallbackFile.Close()
	fallbackTw := tar.NewWriter(fallbackFile)
	defer fallbackTw.Close()

	for _, f := range zr.File {
		tw := resolveWriter(f.Name, opts.Layers, layerWriters, artifactPrefixMap, fallbackTw)
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
	layerWriters []writerState,
	artifactPrefixMap map[string]*tar.Writer,
	fallback *tar.Writer,
) *tar.Writer {
	// Check explicit layers first.
	for i, l := range layers {
		if strings.HasPrefix(name, l.Prefix) {
			return layerWriters[i].tw
		}
	}

	// Check artifact-derived prefixes.
	if artifactPrefixMap != nil {
		for prefix, tw := range artifactPrefixMap {
			if strings.HasPrefix(name, prefix) {
				return tw
			}
		}
	}

	return fallback
}

type writerState struct {
	file *os.File
	tw   *tar.Writer
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
		if f.Name == "META-INF/MANIFEST.MF" {
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
		if strings.HasPrefix(line, "Main-Class:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Main-Class:"))
		}
	}
	return ""
}

// writeEntrypoint generates a shell script that runs the exploded JAR
// using java -cp.
func writeEntrypoint(path, appPrefix, mainClass string) error {
	script := fmt.Sprintf("#!/bin/sh\nexec java ${JAVA_OPTS} -cp %s %s \"$@\"\n", appPrefix, mainClass)
	return os.WriteFile(path, []byte(script), 0755)
}

func writeEntry(tw *tar.Writer, f *zip.File, pathPrefix string) error {
	info := f.FileInfo()
	isDir := info.IsDir() || strings.HasSuffix(f.Name, "/")

	mode := info.Mode()
	if mode == 0 {
		if isDir {
			mode = 0755
		} else {
			mode = 0644
		}
	}

	hdr := &tar.Header{
		Name:    pathPrefix + f.Name,
		ModTime: f.Modified,
		Mode:    int64(mode.Perm()),
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
