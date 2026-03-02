package jartar

import (
	"archive/tar"
	"archive/zip"
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
type Artifact struct {
	ID         string // e.g. "com.google.guava:guava"
	OutputPath string
}

// MavenLockFile represents the relevant parts of the maven lock file JSON.
type MavenLockFile struct {
	Packages map[string][]string `json:"packages"`
}

// SplitOptions configures how a JAR is split into layered tars.
type SplitOptions struct {
	InputPath         string
	FallbackPath      string
	Layers            []Layer
	MavenLockFilePath string
	Artifacts         []Artifact
}

// Split reads a JAR file and distributes entries across layer tars.
// Routing priority: explicit layers first, then artifact-derived prefixes, then fallback.
// All output tars are always written, even if empty.
func Split(opts SplitOptions) error {
	zr, err := zip.OpenReader(opts.InputPath)
	if err != nil {
		return fmt.Errorf("opening jar: %w", err)
	}
	defer zr.Close()

	// Build artifact prefix map if lock file is provided.
	type artifactWriter struct {
		tw *tar.Writer
	}
	var artifactPrefixMap map[string]*tar.Writer // path prefix -> tar writer
	var artifactWriters []writerState

	if opts.MavenLockFilePath != "" && len(opts.Artifacts) > 0 {
		lockFile, err := parseLockFile(opts.MavenLockFilePath)
		if err != nil {
			return err
		}

		artifactPrefixMap = make(map[string]*tar.Writer)
		artifactWriters = make([]writerState, len(opts.Artifacts))

		for i, a := range opts.Artifacts {
			f, err := os.Create(a.OutputPath)
			if err != nil {
				return fmt.Errorf("creating artifact output %s: %w", a.OutputPath, err)
			}
			defer f.Close()
			tw := tar.NewWriter(f)
			defer tw.Close()
			artifactWriters[i] = writerState{file: f, tw: tw}

			// Map each package prefix for this artifact to its tar writer.
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
			return fmt.Errorf("creating layer output %s: %w", l.OutputPath, err)
		}
		defer f.Close()
		tw := tar.NewWriter(f)
		defer tw.Close()
		layerWriters[i] = writerState{file: f, tw: tw}
	}

	// Open fallback tar writer.
	fallbackFile, err := os.Create(opts.FallbackPath)
	if err != nil {
		return fmt.Errorf("creating fallback output: %w", err)
	}
	defer fallbackFile.Close()
	fallbackTw := tar.NewWriter(fallbackFile)
	defer fallbackTw.Close()

	for _, f := range zr.File {
		tw := resolveWriter(f.Name, opts.Layers, layerWriters, artifactPrefixMap, fallbackTw)
		if err := writeEntry(tw, f); err != nil {
			return fmt.Errorf("writing entry %s: %w", f.Name, err)
		}
	}

	return nil
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

func writeEntry(tw *tar.Writer, f *zip.File) error {
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
		Name:    f.Name,
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
