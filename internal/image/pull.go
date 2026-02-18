// Package image provides OCI image pulling, unpacking, and caching.
package image

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// PullResult contains the pulled image and its digest.
type PullResult struct {
	Image  v1.Image
	Digest string // e.g. "sha256:abc123..."
}

// Pull resolves an image reference and pulls the linux/arm64 variant.
func Pull(ctx context.Context, imageRef string) (*PullResult, error) {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return nil, fmt.Errorf("parse image ref %q: %w", imageRef, err)
	}

	platform := &v1.Platform{
		OS:           "linux",
		Architecture: "arm64",
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
			if m.Platform != nil && m.Platform.OS == "linux" && m.Platform.Architecture == "arm64" {
				img, err = idx.Image(m.Digest)
				if err != nil {
					return nil, fmt.Errorf("get arm64 image: %w", err)
				}
				break
			}
		}
		if img == nil {
			return nil, fmt.Errorf("no linux/arm64 variant found in %s", imageRef)
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
		if cfg.OS != "linux" || cfg.Architecture != "arm64" {
			return nil, fmt.Errorf("image %s is %s/%s, aegis requires linux/arm64", imageRef, cfg.OS, cfg.Architecture)
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
