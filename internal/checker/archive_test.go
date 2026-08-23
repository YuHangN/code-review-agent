package checker_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/YuHangN/code-review-agent/internal/checker"
)

func TestExtractArchiveStripsGitHubRootAndRejectsLinks(t *testing.T) {
	archive := tarGzip(t, []tarEntry{{name: "repo-sha/go.mod", body: "module example.test/demo\n"}})
	destination := t.TempDir()
	if err := checker.ExtractArchive(bytes.NewReader(archive), destination); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(destination, "go.mod"))
	if err != nil || string(content) != "module example.test/demo\n" {
		t.Fatalf("content = %q, err = %v", content, err)
	}

	unsafeArchive := tarGzip(t, []tarEntry{{name: "repo-sha/link", kind: tar.TypeSymlink, link: "/etc/passwd"}})
	if err := checker.ExtractArchive(bytes.NewReader(unsafeArchive), t.TempDir()); err == nil {
		t.Fatal("expected symlink archive to be rejected")
	}
}

type tarEntry struct {
	name, body, link string
	kind             byte
}

func tarGzip(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		kind := entry.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		if err := tarWriter.WriteHeader(&tar.Header{Name: entry.name, Typeflag: kind, Linkname: entry.link, Mode: 0o644, Size: int64(len(entry.body))}); err != nil {
			t.Fatal(err)
		}
		if entry.body != "" {
			if _, err := tarWriter.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
