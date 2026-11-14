package fsutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type entry struct {
	name, link, content string
	flag                byte
}

func archive(t *testing.T, entries []entry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.flag, Mode: 0o644, Linkname: e.link, Size: int64(len(e.content))}
		if e.flag == tar.TypeDir {
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.content != "" {
			if _, err := tw.Write([]byte(e.content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	tw.Close()
	gz.Close()
	return &buf
}

func TestExtractTarGzKeepsEverythingInside(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out")
	n, err := ExtractTarGz(archive(t, []entry{
		{name: "repo/", flag: tar.TypeDir},
		{name: "repo/README.md", flag: tar.TypeReg, content: "hi\n"},
		{name: "repo/link-in", flag: tar.TypeSymlink, link: "README.md"},
		{name: "repo/link-out", flag: tar.TypeSymlink, link: "../../secrets"},
		{name: "repo/abs", flag: tar.TypeSymlink, link: "/etc/passwd"},
		{name: "repo/fifo", flag: tar.TypeFifo},
	}), dest, 1<<20)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "repo", "README.md")); string(b) != "hi\n" {
		t.Fatalf("file %q", b)
	}
	if l, err := os.Readlink(filepath.Join(dest, "repo", "link-in")); err != nil || l != "README.md" {
		t.Fatalf("inside link %q %v", l, err)
	}
	for _, skipped := range []string{"link-out", "abs", "fifo"} {
		if _, err := os.Lstat(filepath.Join(dest, "repo", skipped)); err == nil {
			t.Fatalf("%s should have been skipped", skipped)
		}
	}
}

func TestExtractTarGzRefusesEscapesAndOversize(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out")
	_, err := ExtractTarGz(archive(t, []entry{{name: "../evil", flag: tar.TypeReg, content: "x"}}), dest, 1<<20)
	if err == nil {
		t.Fatal("a .. entry must be refused")
	}
	_, err = ExtractTarGz(archive(t, []entry{{name: "/etc/evil", flag: tar.TypeReg, content: "x"}}), dest, 1<<20)
	if err == nil {
		t.Fatal("an absolute entry must be refused")
	}
	_, err = ExtractTarGz(archive(t, []entry{{name: "big", flag: tar.TypeReg, content: string(make([]byte, 100))}}), dest, 50)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "big")); err == nil {
		t.Fatal("the oversize file should have been removed")
	}
	if _, err := ExtractTarGz(bytes.NewReader([]byte("not gzip")), dest, 10); err == nil {
		t.Fatal("not an archive")
	}
}
