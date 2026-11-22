package linssi

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeZip(t *testing.T, files func(w *zip.Writer)) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	files(w)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return p
}

func add(t *testing.T, w *zip.Writer, name string, mode os.FileMode, body string) {
	t.Helper()
	h := &zip.FileHeader{Name: name, Method: zip.Deflate}
	h.SetMode(mode)
	fw, err := w.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte(body))
}

func TestUnzipKeepsModesAndSymlinks(t *testing.T) {
	src := writeZip(t, func(w *zip.Writer) {
		add(t, w, "app/bin/chrome", 0o755, "#!/bin/sh")
		add(t, w, "app/Current", os.ModeSymlink|0o777, "bin")
	})
	dest := t.TempDir()
	if err := unzip(src, dest); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dest, "app/bin/chrome")); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("binary lost its exec bit: %v %v", fi, err)
	}
	if link, err := os.Readlink(filepath.Join(dest, "app/Current")); err != nil || link != "bin" {
		t.Fatalf("symlink: %q %v", link, err)
	}
}

func TestUnzipRefusesEscapes(t *testing.T) {
	for name, files := range map[string]func(w *zip.Writer){
		"path":    func(w *zip.Writer) { add(t, w, "../evil", 0o644, "x") },
		"symlink": func(w *zip.Writer) { add(t, w, "a/link", os.ModeSymlink|0o777, "../../etc") },
	} {
		if err := unzip(writeZip(t, files), t.TempDir()); err == nil || !strings.Contains(err.Error(), "unsafe") {
			t.Errorf("%s escape: got %v", name, err)
		}
	}
}
