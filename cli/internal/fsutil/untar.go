package fsutil

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrTooLarge is returned when an archive would unpack past the caller's byte limit.
var ErrTooLarge = errors.New("archive is larger than allowed")

// ExtractTarGz unpacks a gzip tar into dest, which it creates. The archive is untrusted: entries that would
// land outside dest (absolute paths, `..`, links pointing out) are refused, devices and fifos are skipped,
// and extraction stops once more than maxBytes of file content has been written. Returns how many files
// were written.
func ExtractTarGz(r io.Reader, dest string, maxBytes int64) (int, error) {
	root, err := filepath.Abs(dest)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return 0, err
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var written int64
	files := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return files, fmt.Errorf("reading the archive: %w", err)
		}
		target, ok := inside(root, hdr.Name)
		if !ok {
			return files, fmt.Errorf("archive entry %q would land outside %s", hdr.Name, dest)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return files, err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return files, err
			}
			mode := hdr.FileInfo().Mode().Perm()
			if mode == 0 {
				mode = 0o644
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0o200)
			if err != nil {
				return files, err
			}
			n, err := io.Copy(f, io.LimitReader(tr, maxBytes-written+1))
			f.Close()
			if err != nil {
				return files, err
			}
			written += n
			if written > maxBytes {
				os.Remove(target)
				return files, ErrTooLarge
			}
			files++
		case tar.TypeSymlink:
			// Only links that stay inside the tree: a snapshot of a repo may carry a few, an attacker many.
			if filepath.IsAbs(hdr.Linkname) {
				continue
			}
			if _, ok := inside(root, filepath.Join(filepath.Dir(hdr.Name), hdr.Linkname)); !ok {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return files, err
			}
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return files, err
			}
		case tar.TypeLink:
			src, ok := inside(root, hdr.Linkname)
			if !ok {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return files, err
			}
			os.Remove(target)
			if err := os.Link(src, target); err != nil {
				return files, err
			}
			files++
		default:
			// devices, fifos, extended headers we don't use: skip
		}
	}
}

// inside resolves an archive name under root and reports whether it stays there.
func inside(root, name string) (string, bool) {
	clean := filepath.Clean(strings.ReplaceAll(name, "\\", "/"))
	if clean == "." {
		return root, true
	}
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	target := filepath.Join(root, clean)
	if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
		return "", false
	}
	return target, true
}
