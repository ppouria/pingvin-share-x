package main

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// extractTar unpacks a tar stream into dst. Entries that would leave dst are rejected,
// and symbolic links must point inside dst. Other entry types are refused.
func extractTar(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := safeJoin(dst, hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := writeFile(target, tr, fs.FileMode(hdr.Mode).Perm()|0o600); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := checkLink(dst, target, hdr.Linkname); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("payload entry %s has an unsupported type %q", hdr.Name, hdr.Typeflag)
		}
	}
}

// safeJoin maps an archive name to a path under dst. Clean("/"+name) removes any leading "..".
func safeJoin(dst, name string) string {
	return filepath.Join(dst, filepath.Clean("/"+name))
}

// checkLink rejects absolute links and links that resolve outside dst.
func checkLink(dst, target, linkname string) error {
	if filepath.IsAbs(linkname) {
		return fmt.Errorf("payload link %s is absolute", linkname)
	}
	resolved := filepath.Join(filepath.Dir(target), linkname)
	if resolved != dst && !strings.HasPrefix(resolved, dst+string(os.PathSeparator)) {
		return fmt.Errorf("payload link %s leaves the install directory", linkname)
	}
	return nil
}

func writeFile(path string, r io.Reader, perm fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// copyMissing copies every regular file under src that does not exist under dst yet, so that the
// default images from the payload never overwrite images that an administrator uploaded.
func copyMissing(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if _, err := os.Lstat(target); err == nil {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		info, err := in.Stat()
		if err != nil {
			return err
		}
		return writeFile(target, in, info.Mode().Perm()|0o600)
	})
}
