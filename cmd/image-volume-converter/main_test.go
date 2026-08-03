package main

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// newTestImage builds a small image whose config claims linux/amd64, with two
// gzip-compressed layer blobs, loosely resembling a real exported image.
func newTestImage(t *testing.T) v1.Image {
	t.Helper()
	layer := staticLayer(t)
	img, err := mutate.AppendLayers(empty.Image, layer, layer)
	if err != nil {
		t.Fatalf("AppendLayers: %v", err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("ConfigFile: %v", err)
	}
	cfg.Architecture = "amd64"
	cfg.OS = "linux"
	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		t.Fatalf("mutate.ConfigFile: %v", err)
	}
	return img
}

// TestNoArchLayout exercises the export -> index/manifest/config resolution
// -> config rewrite -> repack flow against a locally synthesized image, so it
// can run without a docker daemon or registry.
func TestNoArchLayout(t *testing.T) {
	workDir := t.TempDir()
	archivePath := filepath.Join(workDir, "docs.tar")
	ociDir := filepath.Join(workDir, "docs-oci")

	img := newTestImage(t)
	if err := exportOCILayout(workDir, archivePath, ociDir, img); err != nil {
		t.Fatalf("exportOCILayout: %v", err)
	}
	if _, err := os.Stat(archivePath); err != nil {
		t.Fatalf("OCI archive not produced: %v", err)
	}

	// The unpacked directory must be a real OCI layout.
	indexPath := filepath.Join(ociDir, "index.json")
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("index.json not unpacked: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ociDir, "oci-layout")); err != nil {
		t.Fatalf("oci-layout marker not unpacked: %v", err)
	}

	// index.json -> manifest -> config, mirroring the manual inspection steps.
	var idx v1.IndexManifest
	if err := readJSON(indexPath, &idx); err != nil {
		t.Fatalf("reading index.json: %v", err)
	}
	if len(idx.Manifests) != 1 {
		t.Fatalf("expected 1 manifest in index.json, got %d", len(idx.Manifests))
	}
	manifestDesc := idx.Manifests[0]
	manifestPath := blobPath(ociDir, manifestDesc.Digest)
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("manifest blob %s not found", manifestDesc.Digest)
	}
	var manifest v1.Manifest
	if err := readJSON(manifestPath, &manifest); err != nil {
		t.Fatalf("reading manifest: %v", err)
	}
	configDesc := manifest.Config
	configPath := blobPath(ociDir, configDesc.Digest)
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config blob %s not found", configDesc.Digest)
	}
	var cfg v1.ConfigFile
	if err := readJSON(configPath, &cfg); err != nil {
		t.Fatalf("reading config: %v", err)
	}
	if cfg.Architecture != "amd64" || cfg.OS != "linux" {
		t.Fatalf("unexpected original config: arch=%q os=%q", cfg.Architecture, cfg.OS)
	}
	origLayerCount := len(manifest.Layers)

	// Rewrite the config and repack into a fresh layout.
	cfg.Architecture = "unknown"
	cfg.OS = "unknown"
	noArch, err := mutate.ConfigFile(img, &cfg)
	if err != nil {
		t.Fatalf("mutate.ConfigFile: %v", err)
	}
	ociNewDir := filepath.Join(workDir, "docs-oci-no-arch")
	if err := writeLayout(ociNewDir, noArch); err != nil {
		t.Fatalf("writeLayout: %v", err)
	}

	outImg, err := imageFromLayout(ociNewDir)
	if err != nil {
		t.Fatalf("imageFromLayout: %v", err)
	}
	outCfg, err := outImg.ConfigFile()
	if err != nil {
		t.Fatalf("outImg.ConfigFile: %v", err)
	}
	if outCfg.Architecture != "unknown" || outCfg.OS != "unknown" {
		t.Fatalf("os/arch not set to unknown: arch=%q os=%q", outCfg.Architecture, outCfg.OS)
	}
	outCfgName, err := outImg.ConfigName()
	if err != nil {
		t.Fatalf("outImg.ConfigName: %v", err)
	}
	if outCfgName == configDesc.Digest {
		t.Fatalf("config digest should have changed after rewrite")
	}

	// The no-arch layout on disk must reference the new manifest/config and
	// keep the original layer blobs untouched.
	outIndexPath := filepath.Join(ociNewDir, "index.json")
	var outIdx v1.IndexManifest
	if err := readJSON(outIndexPath, &outIdx); err != nil {
		t.Fatalf("reading no-arch index.json: %v", err)
	}
	outManifestDesc := outIdx.Manifests[0]
	if outManifestDesc.Digest == manifestDesc.Digest {
		t.Fatalf("index.json should reference a new manifest digest")
	}
	var outManifest v1.Manifest
	if err := readJSON(blobPath(ociNewDir, outManifestDesc.Digest), &outManifest); err != nil {
		t.Fatalf("reading no-arch manifest: %v", err)
	}
	if outManifest.Config.Digest != outCfgName {
		t.Fatalf("manifest references config %s, expected %s", outManifest.Config.Digest, outCfgName)
	}
	if len(outManifest.Layers) != origLayerCount {
		t.Fatalf("layer count changed: got %d, want %d", len(outManifest.Layers), origLayerCount)
	}
	for i, l := range outManifest.Layers {
		if l.Digest != manifest.Layers[i].Digest {
			t.Fatalf("layer %d digest changed: got %s, want %s", i, l.Digest, manifest.Layers[i].Digest)
		}
		if _, err := os.Stat(blobPath(ociNewDir, l.Digest)); err != nil {
			t.Fatalf("layer blob %s missing from no-arch layout: %v", l.Digest, err)
		}
	}
}

// TestPublishToRegistry runs the same no-arch image through remote.Write
// against an in-memory OCI registry and pulls it back, verifying the publish
// path (step 4) end to end.
func TestPublishToRegistry(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()

	img := newTestImage(t)
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("ConfigFile: %v", err)
	}
	cfg.Architecture = "unknown"
	cfg.OS = "unknown"
	noArch, err := mutate.ConfigFile(img, cfg)
	if err != nil {
		t.Fatalf("mutate.ConfigFile: %v", err)
	}

	// Repack through an OCI layout, exactly like the action does.
	ociDir := filepath.Join(t.TempDir(), "oci")
	if err := writeLayout(ociDir, noArch); err != nil {
		t.Fatalf("writeLayout: %v", err)
	}
	outImg, err := imageFromLayout(ociDir)
	if err != nil {
		t.Fatalf("imageFromLayout: %v", err)
	}

	host := strings.TrimPrefix(srv.URL, "http://")
	ref, err := name.ParseReference(fmt.Sprintf("%s/no-arch/test:latest", host))
	if err != nil {
		t.Fatalf("ParseReference: %v", err)
	}
	if err := remote.Write(ref, outImg); err != nil {
		t.Fatalf("remote.Write: %v", err)
	}

	got, err := remote.Image(ref)
	if err != nil {
		t.Fatalf("remote.Image (read back): %v", err)
	}
	gotCfg, err := got.ConfigFile()
	if err != nil {
		t.Fatalf("read-back ConfigFile: %v", err)
	}
	if gotCfg.Architecture != "unknown" || gotCfg.OS != "unknown" {
		t.Fatalf("read-back os/arch not set to unknown: arch=%q os=%q", gotCfg.Architecture, gotCfg.OS)
	}
}

func TestUnpackRejectsZipSlipAndSymlinks(t *testing.T) {
	tempDir := t.TempDir()
	archivePath := filepath.Join(tempDir, "malicious.tar")
	archiveFile, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tw := tar.NewWriter(archiveFile)

	for _, hdr := range []*tar.Header{
		{Name: "safe/ok.txt", Mode: 0o644, Typeflag: tar.TypeReg, Size: int64(len("ok"))},
		{Name: "../../outside.txt", Mode: 0o644, Typeflag: tar.TypeReg, Size: int64(len("bad"))},
		{Name: "link", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "../../etc/passwd"},
	} {
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		switch hdr.Typeflag {
		case tar.TypeReg:
			if hdr.Name == "safe/ok.txt" {
				if _, err := tw.Write([]byte("ok")); err != nil {
					t.Fatalf("Write safe file: %v", err)
				}
			} else {
				if _, err := tw.Write([]byte("bad")); err != nil {
					t.Fatalf("Write bad file: %v", err)
				}
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close tar writer: %v", err)
	}
	if err := archiveFile.Close(); err != nil {
		t.Fatalf("Close archive file: %v", err)
	}

	destDir := filepath.Join(tempDir, "dest")
	if err := unpack(archivePath, destDir); err != nil {
		t.Fatalf("unpack: %v", err)
	}

	if _, err := os.Stat(filepath.Join(destDir, "safe", "ok.txt")); err != nil {
		t.Fatalf("expected safe file to be preserved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destDir, "..", "outside.txt")); !os.IsNotExist(err) {
		t.Fatalf("malicious archive path should not escape destination: stat err=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(destDir, "link")); !os.IsNotExist(err) {
		t.Fatalf("symlink entry should not be extracted: stat err=%v", err)
	}
}

func TestResolveDestination(t *testing.T) {
	cases := []struct {
		name           string
		imageTag       string
		outputImageTag string
		publishTo      string
		wantMode       string
		wantRef        string
		wantPath       string
		wantErr        string
	}{
		{
			name:           "remote push default mode",
			imageTag:       "ghcr.io/kubehub-io/docs:main",
			outputImageTag: "ghcr.io/kubehub-io/docs:no-arch",
			publishTo:      "",
			wantMode:       "RemotePush",
			wantRef:        "ghcr.io/kubehub-io/docs:no-arch",
		},
		{
			name:           "remote push explicit",
			imageTag:       "ghcr.io/kubehub-io/docs:main",
			outputImageTag: "ghcr.io/kubehub-io/docs:no-arch",
			publishTo:      "RemotePush",
			wantMode:       "RemotePush",
			wantRef:        "ghcr.io/kubehub-io/docs:no-arch",
		},
		{
			name:      "remote push defaulted to input tag plus -noarch",
			imageTag:  "ghcr.io/kubehub-io/docs:main",
			publishTo: "RemotePush",
			wantMode:  "RemotePush",
			wantRef:   "ghcr.io/kubehub-io/docs:main-noarch",
		},
		{
			name:      "remote push defaulted on docker.io input",
			imageTag:  "docker.io/kubehub-io/docs:main",
			publishTo: "RemotePush",
			wantMode:  "RemotePush",
			wantRef:   "index.docker.io/kubehub-io/docs:main-noarch",
		},
		{
			name:      "remote push defaulted for input without a tag",
			imageTag:  "ghcr.io/kubehub-io/docs",
			publishTo: "RemotePush",
			wantMode:  "RemotePush",
			wantRef:   "ghcr.io/kubehub-io/docs:latest-noarch",
		},
		{
			name:           "remote push docker.io explicit",
			imageTag:       "docker.io/kubehub-io/docs:main",
			outputImageTag: "docker.io/kubehub-io/docs:no-arch",
			publishTo:      "RemotePush",
			wantMode:       "RemotePush",
			wantRef:        "index.docker.io/kubehub-io/docs:no-arch",
		},
		{
			name:      "oci archive absolute path",
			imageTag:  "ghcr.io/kubehub-io/docs:main",
			publishTo: "OCIArchive:/tmp/docs-no-arch.tar",
			wantMode:  "OCIArchive",
			wantPath:  "/tmp/docs-no-arch.tar",
		},
		{
			name:      "oci archive relative path",
			imageTag:  "ghcr.io/kubehub-io/docs:main",
			publishTo: "OCIArchive:docs-no-arch.tar",
			wantMode:  "OCIArchive",
			wantPath:  "docs-no-arch.tar",
		},
		{
			name:      "oci archive path trimmed",
			imageTag:  "ghcr.io/kubehub-io/docs:main",
			publishTo: " OCIArchive: /tmp/docs-no-arch.tar ",
			wantMode:  "OCIArchive",
			wantPath:  "/tmp/docs-no-arch.tar",
		},
		{
			name:      "oci archive missing path",
			imageTag:  "ghcr.io/kubehub-io/docs:main",
			publishTo: "OCIArchive",
			wantErr:   "requires an output file path",
		},
		{
			name:      "oci archive empty path",
			imageTag:  "ghcr.io/kubehub-io/docs:main",
			publishTo: "OCIArchive:",
			wantErr:   "requires an output file path",
		},
		{
			name:      "invalid publishTo",
			imageTag:  "ghcr.io/kubehub-io/docs:main",
			publishTo: "Registry",
			wantErr:   `invalid publishTo "Registry"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst, err := resolveDestination(tc.imageTag, tc.outputImageTag, tc.publishTo)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("resolveDestination() error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveDestination() unexpected error: %v", err)
			}
			if dst.mode != tc.wantMode {
				t.Fatalf("resolveDestination() mode = %q, want %q", dst.mode, tc.wantMode)
			}
			switch tc.wantMode {
			case "RemotePush":
				if dst.ref == nil || dst.ref.Name() != tc.wantRef {
					t.Fatalf("resolveDestination() ref = %v, want %q", dst.ref, tc.wantRef)
				}
			case "OCIArchive":
				if dst.path != tc.wantPath {
					t.Fatalf("resolveDestination() path = %q, want %q", dst.path, tc.wantPath)
				}
			}
		})
	}
}

// TestExportOCIArchive exports a no-arch OCI layout as a tar archive and
// reads it back to confirm the archive is a valid OCI layout carrying the
// no-arch config.
func TestExportOCIArchive(t *testing.T) {
	img := newTestImage(t)
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("ConfigFile: %v", err)
	}
	cfg.Architecture = "unknown"
	cfg.OS = "unknown"
	noArch, err := mutate.ConfigFile(img, cfg)
	if err != nil {
		t.Fatalf("mutate.ConfigFile: %v", err)
	}

	dir := t.TempDir()
	ociDir := filepath.Join(dir, "oci")
	if err := writeLayout(ociDir, noArch); err != nil {
		t.Fatalf("writeLayout: %v", err)
	}
	archive := filepath.Join(dir, "nested", "out.tar")
	if err := exportOCIArchive(archive, ociDir); err != nil {
		t.Fatalf("exportOCIArchive: %v", err)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("archive not created: %v", err)
	}

	unpacked := filepath.Join(dir, "unpacked")
	if err := unpack(archive, unpacked); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	got, err := imageFromLayout(unpacked)
	if err != nil {
		t.Fatalf("imageFromLayout: %v", err)
	}
	gotCfg, err := got.ConfigFile()
	if err != nil {
		t.Fatalf("ConfigFile: %v", err)
	}
	if gotCfg.Architecture != "unknown" || gotCfg.OS != "unknown" {
		t.Fatalf("os/arch not set to unknown in archive: arch=%q os=%q", gotCfg.Architecture, gotCfg.OS)
	}
}

func TestDefaultRemoteTarget(t *testing.T) {
	t.Run("appends -noarch to the input tag", func(t *testing.T) {
		if got := defaultRemoteTarget("ghcr.io/kubehub-io/docs:main"); got != "ghcr.io/kubehub-io/docs:main-noarch" {
			t.Fatalf("defaultRemoteTarget() = %q, want ghcr.io/kubehub-io/docs:main-noarch", got)
		}
	})
	t.Run("keeps docker.io registry spelling", func(t *testing.T) {
		if got := defaultRemoteTarget("docker.io/kubehub-io/docs:main"); got != "docker.io/kubehub-io/docs:main-noarch" {
			t.Fatalf("defaultRemoteTarget() = %q, want docker.io/kubehub-io/docs:main-noarch", got)
		}
	})
	t.Run("bare source keeps its spelling", func(t *testing.T) {
		if got := defaultRemoteTarget("kubehub-io/docs:main"); got != "kubehub-io/docs:main-noarch" {
			t.Fatalf("defaultRemoteTarget() = %q, want kubehub-io/docs:main-noarch", got)
		}
	})
	t.Run("input without a tag defaults to latest-noarch", func(t *testing.T) {
		if got := defaultRemoteTarget("ghcr.io/kubehub-io/docs"); got != "ghcr.io/kubehub-io/docs:latest-noarch" {
			t.Fatalf("defaultRemoteTarget() = %q, want ghcr.io/kubehub-io/docs:latest-noarch", got)
		}
	})
	t.Run("digest reference falls back to repository no-arch tag", func(t *testing.T) {
		if got := defaultRemoteTarget("ghcr.io/kubehub-io/docs@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); got != "ghcr.io/kubehub-io/docs:no-arch" {
			t.Fatalf("defaultRemoteTarget() = %q, want ghcr.io/kubehub-io/docs:no-arch", got)
		}
	})
	t.Run("invalid reference falls back to no-arch tag", func(t *testing.T) {
		if got := defaultRemoteTarget("not a ref"); got != "not a ref:no-arch" {
			t.Fatalf("defaultRemoteTarget() = %q, want %q", got, "not a ref:no-arch")
		}
	})
}

func staticLayer(t *testing.T) v1.Layer {
	t.Helper()
	return static.NewLayer([]byte("payload"), types.OCILayer)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func blobPath(dir string, h v1.Hash) string {
	return filepath.Join(dir, "blobs", h.Algorithm, h.Hex)
}
