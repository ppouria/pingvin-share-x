package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tarWith(t *testing.T, entries []tar.Header, bodies map[string]string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(bodies[h.Name]))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(bodies[h.Name])); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func TestExtractKeepsModesAndSymlinksInside(t *testing.T) {
	t.Parallel()
	dst := t.TempDir()
	buf := tarWith(t, []tar.Header{
		{Name: "node/bin/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "node/bin/node", Typeflag: tar.TypeReg, Mode: 0o755},
		{Name: "backend/node_modules/.bin/prisma", Typeflag: tar.TypeSymlink, Linkname: "../prisma/build/index.js"},
		{Name: "backend/node_modules/prisma/build/index.js", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"node/bin/node": "#!bin", "backend/node_modules/prisma/build/index.js": "js"})
	if err := extractTar(buf, dst); err != nil {
		t.Fatalf("extract: %v", err)
	}
	info, err := os.Stat(filepath.Join(dst, "node/bin/node"))
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("node binary is not executable: %v %v", info, err)
	}
	link, err := os.Readlink(filepath.Join(dst, "backend/node_modules/.bin/prisma"))
	if err != nil || link != "../prisma/build/index.js" {
		t.Fatalf("symlink = %q, %v", link, err)
	}
}

func TestExtractRejectsEscapingLinks(t *testing.T) {
	t.Parallel()
	for _, link := range []string{"/etc/passwd", "../../../../etc/passwd"} {
		dst := t.TempDir()
		buf := tarWith(t, []tar.Header{{Name: "app/evil", Typeflag: tar.TypeSymlink, Linkname: link}}, nil)
		if err := extractTar(buf, dst); err == nil {
			t.Fatalf("link %q was accepted", link)
		}
	}
}

func TestExtractConfinesNames(t *testing.T) {
	t.Parallel()
	dst := t.TempDir()
	buf := tarWith(t, []tar.Header{{Name: "../../outside.txt", Typeflag: tar.TypeReg, Mode: 0o644}}, map[string]string{"../../outside.txt": "x"})
	if err := extractTar(buf, dst); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "outside.txt")); err != nil {
		t.Fatalf("name was not confined under dst: %v", err)
	}
	if strings.Contains(safeJoin(dst, "../../x"), "..") {
		t.Fatal("safeJoin kept a parent reference")
	}
}

func TestCopyMissingKeepsExistingFiles(t *testing.T) {
	t.Parallel()
	src, dst := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(src, "logo.png"), []byte("default"), 0o644)
	os.WriteFile(filepath.Join(src, "favicon.png"), []byte("default"), 0o644)
	os.WriteFile(filepath.Join(dst, "logo.png"), []byte("uploaded"), 0o644)
	if err := copyMissing(src, dst); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dst, "logo.png"))
	if string(got) != "uploaded" {
		t.Fatalf("existing logo was overwritten: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dst, "favicon.png")); err != nil {
		t.Fatalf("missing default was not copied: %v", err)
	}
}
