package utils

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ZipDirectory archives the contents of sourceDir into zipPath
func ZipDirectory(sourceDir, zipPath string) error {
	zipFile, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer zipFile.Close()

	archive := zip.NewWriter(zipFile)
	defer archive.Close()

	if _, err := os.Stat(sourceDir); err != nil {
		return err
	}

	// Ensure sourceDir ends with a separator so TrimPrefix strips it cleanly
	sourceDirWithSep := filepath.Clean(sourceDir) + string(filepath.Separator)

	filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip the root directory entry itself
		if filepath.Clean(path) == filepath.Clean(sourceDir) {
			return nil
		}

		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}

		// Store files relative to sourceDir (no top-level directory prefix)
		// e.g. "variables.tf" not "terraform-aws-nat/variables.tf"
		header.Name = strings.TrimPrefix(path, sourceDirWithSep)

		if info.IsDir() {
			header.Name += "/"
		} else {
			header.Method = zip.Deflate
		}

		writer, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(writer, file)
		return err
	})

	return err
}
