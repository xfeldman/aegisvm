package image

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	gzip "github.com/klauspost/compress/gzip"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// Unpack extracts all layers of an OCI image into destDir.
// Layers are applied in order (must be sequential for correctness).
// OCI whiteout files (.wh.) are handled.
// Uses klauspost/compress/gzip for ~3-5x faster decompression than stdlib.
func Unpack(img v1.Image, destDir string) error {
	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("get layers: %w", err)
	}

	for i, layer := range layers {
		if err := unpackLayer(layer, destDir); err != nil {
			return fmt.Errorf("unpack layer %d: %w", i, err)
		}
	}

	return nil
}

func unpackLayer(layer v1.Layer, destDir string) error {
	// Use Compressed() + klauspost/gzip instead of layer.Uncompressed()
	// which uses stdlib compress/gzip (~50MB/s). klauspost is 3-5x faster.
	rc, err := layer.Compressed()
	if err != nil {
		return fmt.Errorf("get compressed layer: %w", err)
	}
	defer rc.Close()

	gz, err := gzip.NewReader(rc)
	if err != nil {
		return fmt.Errorf("create gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}

		// Clean the path and ensure it stays within destDir
		cleanName := filepath.Clean(hdr.Name)
		if strings.HasPrefix(cleanName, "..") {
			continue // skip path traversal
		}
		target := filepath.Join(destDir, cleanName)

		// Handle OCI whiteout files
		base := filepath.Base(cleanName)
		dir := filepath.Dir(cleanName)

		if base == ".wh..wh..opq" {
			// Opaque whiteout: remove all contents in this directory
			opqDir := filepath.Join(destDir, dir)
			entries, _ := os.ReadDir(opqDir)
			for _, e := range entries {
				os.RemoveAll(filepath.Join(opqDir, e.Name()))
			}
			continue
		}

		if strings.HasPrefix(base, ".wh.") {
			// File whiteout: remove the corresponding file
			whiteoutTarget := filepath.Join(destDir, dir, strings.TrimPrefix(base, ".wh."))
			os.RemoveAll(whiteoutTarget)
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return fmt.Errorf("mkdir %s: %w", cleanName, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return fmt.Errorf("create %s: %w", cleanName, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("write %s: %w", cleanName, err)
			}
			f.Close()
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			os.Remove(target) // remove existing if any
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("symlink %s -> %s: %w", cleanName, hdr.Linkname, err)
			}
		case tar.TypeLink:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			linkTarget := filepath.Join(destDir, filepath.Clean(hdr.Linkname))
			os.Remove(target)
			if err := os.Link(linkTarget, target); err != nil {
				return fmt.Errorf("hardlink %s -> %s: %w", cleanName, hdr.Linkname, err)
			}
		}
	}

	return nil
}
