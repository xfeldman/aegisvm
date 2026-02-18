package image

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// tarEntry describes a single entry in a tar archive for test building.
type tarEntry struct {
	typeflag byte
	name     string
	content  string   // for regular files
	linkname string   // for symlinks and hardlinks
	mode     int64
}

// buildLayer creates a v1.Layer from a set of tar entries.
func buildLayer(t *testing.T, entries []tarEntry) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     e.mode,
			Linkname: e.linkname,
		}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.content))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header for %s: %v", e.name, err)
		}
		if e.typeflag == tar.TypeReg && len(e.content) > 0 {
			if _, err := tw.Write([]byte(e.content)); err != nil {
				t.Fatalf("write tar content for %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	data := buf.Bytes()
	layer, err := tarball.LayerFromReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("tarball.LayerFromReader: %v", err)
	}
	return layer
}

// buildImage creates a v1.Image from one or more layers.
func buildImage(t *testing.T, layers ...v1.Layer) v1.Image {
	t.Helper()
	adds := make([]mutate.Addendum, len(layers))
	for i, l := range layers {
		adds[i] = mutate.Addendum{Layer: l}
	}
	img, err := mutate.Append(empty.Image, adds...)
	if err != nil {
		t.Fatalf("mutate.Append: %v", err)
	}
	return img
}

func TestUnpack_RegularFiles(t *testing.T) {
	dest := t.TempDir()

	layer := buildLayer(t, []tarEntry{
		{typeflag: tar.TypeDir, name: "etc/", mode: 0755},
		{typeflag: tar.TypeReg, name: "etc/hostname", content: "aegis-vm", mode: 0644},
		{typeflag: tar.TypeReg, name: "hello.txt", content: "world", mode: 0644},
	})
	img := buildImage(t, layer)

	if err := Unpack(img, dest); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	// Verify regular file contents
	data, err := os.ReadFile(filepath.Join(dest, "etc", "hostname"))
	if err != nil {
		t.Fatalf("read etc/hostname: %v", err)
	}
	if string(data) != "aegis-vm" {
		t.Errorf("etc/hostname = %q, want %q", data, "aegis-vm")
	}

	data, err = os.ReadFile(filepath.Join(dest, "hello.txt"))
	if err != nil {
		t.Fatalf("read hello.txt: %v", err)
	}
	if string(data) != "world" {
		t.Errorf("hello.txt = %q, want %q", data, "world")
	}
}

func TestUnpack_DirectoryCreation(t *testing.T) {
	dest := t.TempDir()

	layer := buildLayer(t, []tarEntry{
		{typeflag: tar.TypeDir, name: "a/", mode: 0755},
		{typeflag: tar.TypeDir, name: "a/b/", mode: 0755},
		{typeflag: tar.TypeDir, name: "a/b/c/", mode: 0700},
	})
	img := buildImage(t, layer)

	if err := Unpack(img, dest); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	info, err := os.Stat(filepath.Join(dest, "a", "b", "c"))
	if err != nil {
		t.Fatalf("stat a/b/c: %v", err)
	}
	if !info.IsDir() {
		t.Error("a/b/c should be a directory")
	}
}

func TestUnpack_ImplicitParentDirs(t *testing.T) {
	dest := t.TempDir()

	// File inside a directory that was never explicitly created as a TypeDir entry
	layer := buildLayer(t, []tarEntry{
		{typeflag: tar.TypeReg, name: "deep/nested/dir/file.txt", content: "deep", mode: 0644},
	})
	img := buildImage(t, layer)

	if err := Unpack(img, dest); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dest, "deep", "nested", "dir", "file.txt"))
	if err != nil {
		t.Fatalf("read deep/nested/dir/file.txt: %v", err)
	}
	if string(data) != "deep" {
		t.Errorf("content = %q, want %q", data, "deep")
	}
}

func TestUnpack_Symlink(t *testing.T) {
	dest := t.TempDir()

	layer := buildLayer(t, []tarEntry{
		{typeflag: tar.TypeReg, name: "target.txt", content: "original", mode: 0644},
		{typeflag: tar.TypeSymlink, name: "link.txt", linkname: "target.txt"},
	})
	img := buildImage(t, layer)

	if err := Unpack(img, dest); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	linkTarget, err := os.Readlink(filepath.Join(dest, "link.txt"))
	if err != nil {
		t.Fatalf("readlink link.txt: %v", err)
	}
	if linkTarget != "target.txt" {
		t.Errorf("symlink target = %q, want %q", linkTarget, "target.txt")
	}

	// Reading through symlink should work
	data, err := os.ReadFile(filepath.Join(dest, "link.txt"))
	if err != nil {
		t.Fatalf("read through symlink: %v", err)
	}
	if string(data) != "original" {
		t.Errorf("content via symlink = %q, want %q", data, "original")
	}
}

func TestUnpack_Hardlink(t *testing.T) {
	dest := t.TempDir()

	layer := buildLayer(t, []tarEntry{
		{typeflag: tar.TypeReg, name: "original.txt", content: "shared", mode: 0644},
		{typeflag: tar.TypeLink, name: "hardlink.txt", linkname: "original.txt"},
	})
	img := buildImage(t, layer)

	if err := Unpack(img, dest); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dest, "hardlink.txt"))
	if err != nil {
		t.Fatalf("read hardlink.txt: %v", err)
	}
	if string(data) != "shared" {
		t.Errorf("hardlink content = %q, want %q", data, "shared")
	}

	// Both files should have the same inode (hardlink)
	origInfo, _ := os.Stat(filepath.Join(dest, "original.txt"))
	linkInfo, _ := os.Stat(filepath.Join(dest, "hardlink.txt"))
	if !os.SameFile(origInfo, linkInfo) {
		t.Error("expected original.txt and hardlink.txt to be the same file (hardlink)")
	}
}

func TestUnpack_WhiteoutFile(t *testing.T) {
	dest := t.TempDir()

	// Layer 1: create a file
	layer1 := buildLayer(t, []tarEntry{
		{typeflag: tar.TypeDir, name: "etc/", mode: 0755},
		{typeflag: tar.TypeReg, name: "etc/remove-me.conf", content: "old config", mode: 0644},
		{typeflag: tar.TypeReg, name: "etc/keep-me.conf", content: "keep", mode: 0644},
	})

	// Layer 2: whiteout the file (OCI whiteout marker)
	layer2 := buildLayer(t, []tarEntry{
		{typeflag: tar.TypeReg, name: "etc/.wh.remove-me.conf", content: "", mode: 0644},
	})

	img := buildImage(t, layer1, layer2)

	if err := Unpack(img, dest); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	// The whiteout target should not exist
	if _, err := os.Stat(filepath.Join(dest, "etc", "remove-me.conf")); !os.IsNotExist(err) {
		t.Error("etc/remove-me.conf should have been removed by whiteout")
	}

	// The other file should still exist
	data, err := os.ReadFile(filepath.Join(dest, "etc", "keep-me.conf"))
	if err != nil {
		t.Fatalf("read etc/keep-me.conf: %v", err)
	}
	if string(data) != "keep" {
		t.Errorf("etc/keep-me.conf = %q, want %q", data, "keep")
	}
}

func TestUnpack_OpaqueWhiteout(t *testing.T) {
	dest := t.TempDir()

	// Layer 1: create a directory with files
	layer1 := buildLayer(t, []tarEntry{
		{typeflag: tar.TypeDir, name: "var/", mode: 0755},
		{typeflag: tar.TypeDir, name: "var/cache/", mode: 0755},
		{typeflag: tar.TypeReg, name: "var/cache/old1.txt", content: "old1", mode: 0644},
		{typeflag: tar.TypeReg, name: "var/cache/old2.txt", content: "old2", mode: 0644},
	})

	// Layer 2: opaque whiteout wipes the directory, then adds new content
	layer2 := buildLayer(t, []tarEntry{
		{typeflag: tar.TypeReg, name: "var/cache/.wh..wh..opq", content: "", mode: 0644},
		{typeflag: tar.TypeReg, name: "var/cache/new.txt", content: "new", mode: 0644},
	})

	img := buildImage(t, layer1, layer2)

	if err := Unpack(img, dest); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	// Old files should be gone (opaque whiteout removes all contents)
	if _, err := os.Stat(filepath.Join(dest, "var", "cache", "old1.txt")); !os.IsNotExist(err) {
		t.Error("var/cache/old1.txt should have been removed by opaque whiteout")
	}
	if _, err := os.Stat(filepath.Join(dest, "var", "cache", "old2.txt")); !os.IsNotExist(err) {
		t.Error("var/cache/old2.txt should have been removed by opaque whiteout")
	}

	// New file from the same layer should exist
	data, err := os.ReadFile(filepath.Join(dest, "var", "cache", "new.txt"))
	if err != nil {
		t.Fatalf("read var/cache/new.txt: %v", err)
	}
	if string(data) != "new" {
		t.Errorf("var/cache/new.txt = %q, want %q", data, "new")
	}
}

func TestUnpack_PathTraversalSkipped(t *testing.T) {
	dest := t.TempDir()

	// A malicious layer with path traversal should be skipped
	layer := buildLayer(t, []tarEntry{
		{typeflag: tar.TypeReg, name: "../../../etc/passwd", content: "evil", mode: 0644},
		{typeflag: tar.TypeReg, name: "safe.txt", content: "safe", mode: 0644},
	})
	img := buildImage(t, layer)

	if err := Unpack(img, dest); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	// The traversal entry should have been skipped
	if _, err := os.Stat(filepath.Join(dest, "..", "..", "..", "etc", "passwd")); err == nil {
		t.Error("path traversal entry should have been skipped")
	}

	// The safe file should exist
	data, err := os.ReadFile(filepath.Join(dest, "safe.txt"))
	if err != nil {
		t.Fatalf("read safe.txt: %v", err)
	}
	if string(data) != "safe" {
		t.Errorf("safe.txt = %q, want %q", data, "safe")
	}
}

func TestUnpack_MultipleLayers(t *testing.T) {
	dest := t.TempDir()

	// Layer 1: base file
	layer1 := buildLayer(t, []tarEntry{
		{typeflag: tar.TypeReg, name: "config.txt", content: "v1", mode: 0644},
		{typeflag: tar.TypeReg, name: "base.txt", content: "base", mode: 0644},
	})

	// Layer 2: overwrite file
	layer2 := buildLayer(t, []tarEntry{
		{typeflag: tar.TypeReg, name: "config.txt", content: "v2", mode: 0644},
	})

	img := buildImage(t, layer1, layer2)

	if err := Unpack(img, dest); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	// config.txt should have layer 2 content (overwrite)
	data, err := os.ReadFile(filepath.Join(dest, "config.txt"))
	if err != nil {
		t.Fatalf("read config.txt: %v", err)
	}
	if string(data) != "v2" {
		t.Errorf("config.txt = %q, want %q (layer 2 should overwrite layer 1)", data, "v2")
	}

	// base.txt should still exist from layer 1
	data, err = os.ReadFile(filepath.Join(dest, "base.txt"))
	if err != nil {
		t.Fatalf("read base.txt: %v", err)
	}
	if string(data) != "base" {
		t.Errorf("base.txt = %q, want %q", data, "base")
	}
}

func TestUnpack_EmptyImage(t *testing.T) {
	dest := t.TempDir()

	// An image with no layers should unpack without error
	img := buildImage(t)

	if err := Unpack(img, dest); err != nil {
		t.Fatalf("Unpack empty image: %v", err)
	}

	// dest should remain empty
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatalf("read dest dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty directory, got %d entries", len(entries))
	}
}

func TestUnpack_RegularFileWritesThroughSymlink(t *testing.T) {
	dest := t.TempDir()

	// Layer 1: create a symlink pointing to a real file
	layer1 := buildLayer(t, []tarEntry{
		{typeflag: tar.TypeReg, name: "real.txt", content: "real", mode: 0644},
		{typeflag: tar.TypeSymlink, name: "link.txt", linkname: "real.txt"},
	})

	// Layer 2: write a regular file at the symlink path.
	// OpenFile follows symlinks, so the write goes through to real.txt.
	layer2 := buildLayer(t, []tarEntry{
		{typeflag: tar.TypeReg, name: "link.txt", content: "updated via link", mode: 0644},
	})

	img := buildImage(t, layer1, layer2)

	if err := Unpack(img, dest); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	// The symlink itself should still be a symlink (OpenFile follows it)
	info, err := os.Lstat(filepath.Join(dest, "link.txt"))
	if err != nil {
		t.Fatalf("lstat link.txt: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("link.txt should still be a symlink (OpenFile follows symlinks)")
	}

	// The underlying file should have the new content
	data, err := os.ReadFile(filepath.Join(dest, "real.txt"))
	if err != nil {
		t.Fatalf("read real.txt: %v", err)
	}
	if string(data) != "updated via link" {
		t.Errorf("real.txt = %q, want %q", data, "updated via link")
	}
}

func TestUnpack_WhiteoutDirectory(t *testing.T) {
	dest := t.TempDir()

	// Layer 1: create a directory with contents
	layer1 := buildLayer(t, []tarEntry{
		{typeflag: tar.TypeDir, name: "lib/", mode: 0755},
		{typeflag: tar.TypeDir, name: "lib/old/", mode: 0755},
		{typeflag: tar.TypeReg, name: "lib/old/data.txt", content: "data", mode: 0644},
	})

	// Layer 2: whiteout the entire directory
	layer2 := buildLayer(t, []tarEntry{
		{typeflag: tar.TypeReg, name: "lib/.wh.old", content: "", mode: 0644},
	})

	img := buildImage(t, layer1, layer2)

	if err := Unpack(img, dest); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	// The directory and its contents should be gone
	if _, err := os.Stat(filepath.Join(dest, "lib", "old")); !os.IsNotExist(err) {
		t.Error("lib/old directory should have been removed by whiteout")
	}

	// The parent directory should still exist
	info, err := os.Stat(filepath.Join(dest, "lib"))
	if err != nil {
		t.Fatalf("stat lib: %v", err)
	}
	if !info.IsDir() {
		t.Error("lib should still be a directory")
	}
}
