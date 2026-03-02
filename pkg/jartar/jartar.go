package jartar

import (
	"archive/tar"
	"archive/zip"
	"fmt"
	"io"
	"os"
	"strings"
)

// Convert reads a JAR file (zip format) at inputPath and writes a tar
// archive to outputPath, preserving all file names and directory structure.
func Convert(inputPath, outputPath string) error {
	zr, err := zip.OpenReader(inputPath)
	if err != nil {
		return fmt.Errorf("opening jar: %w", err)
	}
	defer zr.Close()

	outFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("creating output: %w", err)
	}
	defer outFile.Close()

	tw := tar.NewWriter(outFile)
	defer tw.Close()

	for _, f := range zr.File {
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
