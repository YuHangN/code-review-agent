package checker

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	maxArchiveFiles = 100_000
	maxArchiveBytes = int64(512 << 20)
)

// ExtractArchive 安全解压 GitHub 固定 SHA tarball，并去掉归档自带的顶层目录。
func ExtractArchive(input io.Reader, destination string) error {
	gzipReader, err := gzip.NewReader(input)
	if err != nil {
		return fmt.Errorf("open repository archive: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	files := 0
	var total int64
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read repository archive: %w", err)
		}
		files++
		total += header.Size
		if files > maxArchiveFiles || total > maxArchiveBytes {
			return fmt.Errorf("repository archive exceeds safety limit")
		}
		clean := path.Clean(header.Name)
		parts := strings.Split(clean, "/")
		if clean == "." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) || len(parts) < 2 {
			return fmt.Errorf("unsafe repository archive path")
		}
		relative := path.Join(parts[1:]...)
		target := filepath.Join(destination, filepath.FromSlash(relative))
		if !strings.HasPrefix(target, filepath.Clean(destination)+string(os.PathSeparator)) {
			return fmt.Errorf("repository archive path escapes destination")
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("create archive directory: %w", err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("create archive parent: %w", err)
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err != nil {
				return fmt.Errorf("create archive file: %w", err)
			}
			_, copyErr := io.CopyN(file, tarReader, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("write archive file: %w", copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close archive file: %w", closeErr)
			}
		default:
			return fmt.Errorf("repository archive contains unsupported entry")
		}
	}
	return nil
}
