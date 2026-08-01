// Command image-volume-converter implements the "OCI image arch/os sanitizer"
// GitHub Action. It loads an image (preferring the local docker daemon,
// otherwise pulling it for the platform of the runner), exports it as an OCI
// archive and unpacks it into an OCI layout directory, rewrites the image
// config so that `architecture` and `os` are `unknown`, and finally publishes
// the resulting image to a ghcr.io repository.
package main

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "image-volume-converter:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	imageTag := os.Getenv("INPUT_IMAGE_TAG")
	publishTag := os.Getenv("INPUT_PUBLISH_IMAGE_TAG")
	username := os.Getenv("INPUT_REGISTRY_USERNAME")
	password := os.Getenv("INPUT_REGISTRY_PASSWORD")
	actor := os.Getenv("INPUT_GITHUB_ACTOR")
	token := os.Getenv("INPUT_GITHUB_TOKEN")

	if imageTag == "" {
		return errors.New("INPUT_IMAGE_TAG is required")
	}
	if publishTag == "" {
		return errors.New("INPUT_PUBLISH_IMAGE_TAG is required")
	}
	if !strings.HasPrefix(publishTag, "ghcr.io/") {
		return fmt.Errorf("publishImageTag %q must be under ghcr.io", publishTag)
	}
	if username == "" {
		username = actor
	}
	if password == "" {
		password = token
	}
	if password == "" {
		fmt.Fprintln(os.Stderr, "image-volume-converter: warning: no registry credentials provided; publishing to ghcr.io will fail unless the repository is public")
	}

	srcRef, err := name.ParseReference(imageTag)
	if err != nil {
		return fmt.Errorf("parsing imageTag %q: %w", imageTag, err)
	}
	dstRef, err := name.ParseReference(publishTag)
	if err != nil {
		return fmt.Errorf("parsing publishImageTag %q: %w", publishTag, err)
	}

	auth := authn.Anonymous
	if password != "" {
		auth = authn.FromConfig(authn.AuthConfig{Username: username, Password: password})
	}

	platform, err := runnerPlatform()
	if err != nil {
		return err
	}

	// 1) Load the image: prefer the local docker daemon, otherwise pull it for
	//    the platform of the current runner.
	img, source, err := loadImage(ctx, srcRef, platform, auth)
	if err != nil {
		return err
	}
	fmt.Printf("image-volume-converter: resolved %s from %s\n", srcRef, source)

	// 1b) Export the image as an OCI archive and unpack it into an OCI layout
	//     directory. This mirrors:
	//       podman save --format oci-archive -o /tmp/docs.tar <image>
	//       tar -xf /tmp/docs.tar -C /tmp/docs-oci
	workDir := filepath.Join(os.TempDir(), "image-volume-converter")
	archivePath := filepath.Join(workDir, "docs.tar")
	ociDir := filepath.Join(workDir, "docs-oci")
	if err := exportOCILayout(workDir, archivePath, ociDir, img); err != nil {
		return err
	}

	// 2) Resolve index.json -> manifest -> config, reading the blobs exactly as
	//    if the unpacked directory had been inspected by hand.
	lp, err := layout.FromPath(ociDir)
	if err != nil {
		return fmt.Errorf("reading OCI layout %s: %w", ociDir, err)
	}
	ii, err := lp.ImageIndex()
	if err != nil {
		return fmt.Errorf("reading OCI index: %w", err)
	}
	idx, err := ii.IndexManifest()
	if err != nil {
		return fmt.Errorf("reading index.json: %w", err)
	}
	if len(idx.Manifests) == 0 {
		return errors.New("index.json contains no manifests")
	}
	manifestDesc := idx.Manifests[0]
	manifestImg, err := ii.Image(manifestDesc.Digest)
	if err != nil {
		return fmt.Errorf("reading manifest %s: %w", manifestDesc.Digest, err)
	}
	configName, err := manifestImg.ConfigName()
	if err != nil {
		return err
	}
	cfg, err := manifestImg.ConfigFile()
	if err != nil {
		return fmt.Errorf("reading config %s: %w", configName, err)
	}
	fmt.Printf("image-volume-converter: index.json -> manifest %s -> config %s (architecture=%q os=%q)\n",
		manifestDesc.Digest, configName, cfg.Architecture, cfg.OS)

	// 3) Rewrite the config: architecture and os both become "unknown", then
	//    repack into a fresh OCI layout (new config blob, manifest and index).
	fmt.Printf("image-volume-converter: setting architecture and os to \"unknown\"\n")
	cfg.Architecture = "unknown"
	cfg.OS = "unknown"
	sanitized, err := mutate.ConfigFile(manifestImg, cfg)
	if err != nil {
		return fmt.Errorf("updating config: %w", err)
	}

	ociNewDir := filepath.Join(workDir, "docs-image-volume-converterd")
	if err := writeLayout(ociNewDir, sanitized); err != nil {
		return err
	}
	outImg, err := imageFromLayout(ociNewDir)
	if err != nil {
		return fmt.Errorf("reading sanitized layout: %w", err)
	}
	outCfg, err := outImg.ConfigFile()
	if err != nil {
		return err
	}
	if outCfg.Architecture != "unknown" || outCfg.OS != "unknown" {
		return errors.New("config was not sanitized correctly")
	}
	newConfigName, err := outImg.ConfigName()
	if err != nil {
		return err
	}
	fmt.Printf("image-volume-converter: sanitized layout written to %s (config %s, architecture=%q os=%q)\n",
		ociNewDir, newConfigName, outCfg.Architecture, outCfg.OS)

	// 4) Publish the packed layout to the target registry (ghcr.io).
	fmt.Printf("image-volume-converter: publishing %s -> %s\n", srcRef, dstRef)
	if err := remote.Write(dstRef, outImg,
		remote.WithAuth(auth),
		remote.WithContext(ctx),
	); err != nil {
		return fmt.Errorf("publishing image: %w", err)
	}
	fmt.Printf("image-volume-converter: published %s\n", dstRef.Name())
	return nil
}

// loadImage returns the image from the local docker daemon when available,
// falling back to pulling it from its registry for the given platform.
func loadImage(ctx context.Context, ref name.Reference, platform v1.Platform, auth authn.Authenticator) (v1.Image, string, error) {
	if img, err := daemon.Image(ref); err == nil {
		return img, "local docker daemon", nil
	}
	fmt.Printf("image-volume-converter: image not found in local docker daemon, pulling %s (platform %s/%s)\n",
		ref, platform.OS, platform.Architecture)
	img, err := remote.Image(ref,
		remote.WithPlatform(platform),
		remote.WithAuth(auth),
		remote.WithContext(ctx),
	)
	if err != nil {
		return nil, "", fmt.Errorf("pulling image %s: %w", ref, err)
	}
	return img, "remote registry", nil
}

// exportOCILayout writes the image into an OCI layout directory, packs it into
// an OCI archive and then unpacks the archive into the final directory.
func exportOCILayout(workDir, archivePath, ociDir string, img v1.Image) error {
	if err := os.RemoveAll(workDir); err != nil {
		return err
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return err
	}

	staging := filepath.Join(workDir, "staging")
	if err := writeLayout(staging, img); err != nil {
		return err
	}
	if err := packDir(staging, archivePath); err != nil {
		return err
	}
	if err := unpack(archivePath, ociDir); err != nil {
		return err
	}
	fmt.Printf("image-volume-converter: exported OCI archive to %s and unpacked it to %s\n", archivePath, ociDir)
	return nil
}

// writeLayout creates a fresh OCI layout directory containing the single image.
func writeLayout(dir string, img v1.Image) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	path, err := layout.Write(dir, empty.Index)
	if err != nil {
		return err
	}
	return path.AppendImage(img)
}

// imageFromLayout resolves the single image described by an OCI layout
// directory's index.json (index.json -> manifest -> image).
func imageFromLayout(dir string) (v1.Image, error) {
	lp, err := layout.FromPath(dir)
	if err != nil {
		return nil, err
	}
	ii, err := lp.ImageIndex()
	if err != nil {
		return nil, err
	}
	idx, err := ii.IndexManifest()
	if err != nil {
		return nil, err
	}
	if len(idx.Manifests) == 0 {
		return nil, errors.New("layout contains no manifests")
	}
	return ii.Image(idx.Manifests[0].Digest)
}

// packDir creates a tar archive of srcDir with paths relative to srcDir (the
// on-disk representation of an OCI archive).
func packDir(srcDir, archivePath string) error {
	f, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	defer tw.Close()

	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, src)
		src.Close()
		return copyErr
	})
}

// unpack extracts a tar archive into destDir, guarding against path traversal.
func unpack(archivePath, destDir string) error {
	if err := os.RemoveAll(destDir); err != nil {
		return err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}

	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hdr.Name)
		if name == "." || filepath.IsAbs(name) || strings.HasPrefix(name, "..") {
			continue
		}
		target := filepath.Join(destDir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, tr)
			out.Close()
			if copyErr != nil {
				return copyErr
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		}
	}
	return nil
}

// runnerPlatform maps the architecture of the current runner to the OCI
// platform used when pulling a multi-arch image.
func runnerPlatform() (v1.Platform, error) {
	switch runtime.GOARCH {
	case "amd64":
		return v1.Platform{OS: "linux", Architecture: "amd64"}, nil
	case "arm64":
		return v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}, nil
	case "arm":
		return v1.Platform{OS: "linux", Architecture: "arm", Variant: "v7"}, nil
	case "386":
		return v1.Platform{OS: "linux", Architecture: "386"}, nil
	default:
		return v1.Platform{}, fmt.Errorf("unsupported runner architecture %q", runtime.GOARCH)
	}
}
