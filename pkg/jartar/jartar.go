package jartar

import (
	"archive/tar"
	"archive/zip"
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

// Split reads a JAR file and distributes entries across layer tars by prefix
// match. Entries not matching any layer prefix go to fallbackPath. All output
// tars are always written, even if empty.
func Split(inputPath, fallbackPath string, layers []Layer) error {
	zr, err := zip.OpenReader(inputPath)
	if err != nil {
		return fmt.Errorf("opening jar: %w", err)
	}
	defer zr.Close()

	// Open all tar writers up front.
	type writerState struct {
		file *os.File
		tw   *tar.Writer
	}

	writers := make([]writerState, len(layers))
	for i, l := range layers {
		f, err := os.Create(l.OutputPath)
		if err != nil {
			return fmt.Errorf("creating layer output %s: %w", l.OutputPath, err)
		}
		defer f.Close()
		tw := tar.NewWriter(f)
		defer tw.Close()
		writers[i] = writerState{file: f, tw: tw}
	}

	fallbackFile, err := os.Create(fallbackPath)
	if err != nil {
		return fmt.Errorf("creating fallback output: %w", err)
	}
	defer fallbackFile.Close()
	fallbackTw := tar.NewWriter(fallbackFile)
	defer fallbackTw.Close()

	for _, f := range zr.File {
		tw := fallbackTw
		for i, l := range layers {
			if strings.HasPrefix(f.Name, l.Prefix) {
				tw = writers[i].tw
				break
			}
		}
		if err := writeEntry(tw, f); err != nil {
			return fmt.Errorf("writing entry %s: %w", f.Name, err)
		}
	}

	return nil
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
