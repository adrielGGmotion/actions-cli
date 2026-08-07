package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSafeJoin(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"../x", "/x", "a/../../x", ""} {
		if _, err := safeJoin(root, p); err == nil {
			t.Fatalf("accepted %q", p)
		}
	}
	if _, err := safeJoin(root, "dir/file with spaces"); err != nil {
		t.Fatal(err)
	}
}

func TestAtomicOutputRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skip(err)
	}
	h := &tar.Header{Size: 1, Mode: 0644}
	if err := writeTarFileAtomic(filepath.Join(root, "link", "x"), bytes.NewReader([]byte("x")), h, 10); err == nil {
		t.Fatal("accepted symlink parent")
	}
}

func TestPatternSafety(t *testing.T) {
	if safePattern("../secret") || safePattern("/tmp/x") {
		t.Fatal("unsafe pattern accepted")
	}
	if !safePattern("app/build/**") {
		t.Fatal("safe pattern rejected")
	}
}
