// Package image provides OCI image pulling, unpacking, and caching.
package image

import (
	"context"
	"fmt"
	"runtime"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// vmArch returns the guest architecture for OCI image pulls.
// On macOS, VMs are always arm64 (libkrun on Apple Silicon).
// On Linux, VMs match the host architecture (Cloud Hypervisor runs native).
func vmArch() string {
	if runtime.GOOS == "darwin" {
		return "arm64"
	}
	return runtime.GOARCH
}

// PullResult contains the pulled image and its digest.
type PullResult struct {
	Image  v1.Image
	Digest string // e.g. "sha256:abc123..."
}

// Pull resolves an image reference and pulls the linux variant matching the VM architecture.
func Pull(ctx context.Context, imageRef string) (*PullResult, error) {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return nil, fmt.Errorf("parse image ref %q: %w", imageRef, err)
	}

	arch := vmArch()
	platform := &v1.Platform{
		OS:           "linux",
		Architecture: arch,
	}

	desc, err := remote.Get(ref, remote.WithContext(ctx), remote.WithPlatform(*platform))
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", imageRef, err)
	}

	var img v1.Image

	switch desc.MediaType {
	case types.OCIImageIndex, types.DockerManifestList:
		idx, err := desc.ImageIndex()
		if err != nil {
			return nil, fmt.Errorf("get image index: %w", err)
		}
		indexManifest, err := idx.IndexManifest()
		if err != nil {
			return nil, fmt.Errorf("get index manifest: %w", err)
		}
		for _, m := range indexManifest.Manifests {
			if m.Platform != nil && m.Platform.OS == "linux" && m.Platform.Architecture == arch {
				img, err = idx.Image(m.Digest)
				if err != nil {
					return nil, fmt.Errorf("get %s image: %w", arch, err)
				}
				break
			}
		}
		if img == nil {
			return nil, fmt.Errorf("no linux/%s variant found in %s", arch, imageRef)
		}
	default:
		img, err = desc.Image()
		if err != nil {
			return nil, fmt.Errorf("get image: %w", err)
		}
		// Single-manifest image — verify it's actually linux/arm64.
		// Without this check, a linux/amd64 image unpacks fine but
		// fails at VM boot with confusing exec format errors.
		cfg, err := img.ConfigFile()
		if err != nil {
			return nil, fmt.Errorf("get image config: %w", err)
		}
		if cfg.OS != "linux" || cfg.Architecture != arch {
			return nil, fmt.Errorf("image %s is %s/%s, aegis requires linux/%s", imageRef, cfg.OS, cfg.Architecture, arch)
		}
	}

	digest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("get digest: %w", err)
	}

	return &PullResult{
		Image:  img,
		Digest: digest.String(),
	}, nil
}
