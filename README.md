# Image Volume Converter

A GitHub Action (and standalone binary) that re-publishes a container image
with its `architecture` and `os` config fields set to `unknown`, so the image
can be used as a Kubernetes **image volume**.

## The problem

Kubernetes image volumes (`kubernetes.io/image`) let you expose a container
image to the node as a read-only volume, without running the container. It is a
great fit for content that is purely data: static sites, caches, models,
binaries, and anything else that does not "run".

There are three things that get in the way:

1. **The kubelet rejects the image.** Before mounting an image volume, the
   kubelet validates the image against the node's platform. If your image
   declares, say, `linux/amd64` and the node is an ARM machine, the volume fails
   to mount with an architecture/os mismatch error. The error reads like a bug:
   there is nothing to execute, so no architecture should matter — but the
   kubelet refuse to pull.

2. **Building a multi-arch image feels wrong.** The standard advice is to build
   the image for every platform you might target. But that is solving a problem
   that should not exist: your content has no architecture. Why maintain a
   matrix of builds just to satisfy a kubelet check on data that is
   architecture-independent?

3. **Your content is not your runtime.** Serving a Node.js, Python, or Java app?
   Split the app content from the runtime and ship the content on its own. The
   runtime is architecture-specific, but the content — your static files, non-machine code (`.js`, `.min.js`, `.jar`, `.py`),
   models, and assets — is platform-neutral. Once the two are separated, the
   content-only image has no reason to carry an architecture at all.

The workaround is to publish the image once, with `architecture` and `os`
declared as `unknown`. The kubelet treats such images as platform-neutral, and
the volume mounts on any node. This is exactly what **image-volume-converter**
produces.

## How it works

`image-volume-converter` takes an already-built image, rewrites its OCI config
so that `architecture` and `os` are `unknown`, and either pushes the result to a
remote registry or writes it as an OCI archive to a local file. It works the
same way whether you use it as a GitHub Action or as a standalone binary:

1. Load the source image — from the local docker daemon if available, otherwise
   by pulling it from its registry for the platform of the current machine.
2. Export the image as an OCI archive and unpack it into an OCI layout.
3. Resolve `index.json → manifest → config`, set `architecture` and `os` to
   `unknown`, and repack a fresh layout.
4. Push the converted image to a remote registry, or write it as an OCI archive
   to a local file.

Only the image config is rewritten; the layer blobs are reused as-is.

> **Docker vs containerd/kubelet:** the converted image declares `os`/`arch` as
> `unknown`. Docker's classic image store refuses to load such images, but
> **containerd and the Kubernetes kubelet accept them** — which is exactly what
> makes image volumes work on any node platform.

## Usage as a GitHub Action

Add a job to your workflow after the image has been built. The action needs no
extra permissions beyond what you already use to push images to ghcr.io.

```yaml
name: Publish sanitized image for Kubernetes image volumes

on:
  push:
    branches: [main]

permissions:
  contents: read
  packages: write

jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout repository
        uses: actions/checkout@v7

      - name: Build the image
        uses: docker/build-push-action@v4
        with:
          context: .
          file: ./test/Dockerfile
          load: true
          tags: ghcr.io/${{ github.repository }}-test:main-latest

      - name: Sanitize and publish image
        uses: kubehub-io/image-volume@v1
        with:
          imageTag: ghcr.io/${{ github.repository }}-test:main-latest
          outputImageTag: ghcr.io/${{ github.repository }}-test:sanitized
```

### publishTo

`publishTo` controls where the converted image goes:

| Value                   | Behavior                                                                                                   |
| ----------------------- | ---------------------------------------------------------------------------------------------------------- |
| `RemotePush`            | Push the converted image to a remote registry (`outputImageTag`). **Default.**                            |
| `OCIArchive:<path>`     | Write an OCI archive (a tar of an OCI layout) to the local file `<path>` instead.                          |

To push the converted image to ghcr.io, use the default `RemotePush`:

```yaml
      - name: Sanitize and push to ghcr.io
        uses: kubehub-io/image-volume@v1
        with:
          imageTag: ghcr.io/${{ github.repository }}-test:main-latest
          outputImageTag: ghcr.io/${{ github.repository }}-test:sanitized
```

To get an OCI archive on the runner instead of pushing anywhere — e.g. to
inspect it with `skopeo`/`regctl`, or to ship it to an on-prem cluster — use
`OCIArchive:<path>`. The path is a local file the archive is written to;
`outputImageTag` is ignored:

```yaml
      - name: Sanitize and export OCI archive
        uses: kubehub-io/image-volume@v1
        with:
          imageTag: ghcr.io/${{ github.repository }}-test:main-latest
          publishTo: OCIArchive:${{ runner.temp }}/sanitized.tar

      - name: Inspect the converted image
        run: |
          skopeo inspect "oci-archive:${{ runner.temp }}/sanitized.tar" \
            --format 'architecture={{.Architecture}} os={{.Os}}'
```

> **Note:** Docker's classic image store refuses to load images whose `os`/`arch`
> do not match the daemon host (moby's `image.CheckOS`), and the converted image
> declares `os`/`arch` as `unknown`. It therefore cannot be `docker load`-ed;
> use an OCI archive (inspectable with `skopeo`/`regctl`, importable with
> `ctr images import`) or push it to a registry. containerd and the Kubernetes
> kubelet accept the sanitized image.

### Inputs

| Input               | Required | Default          | Description                                                                                                                                                                                     |
| ------------------- | :------: | ---------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `imageTag`          |   yes    | —                | The source image to convert, e.g. `ghcr.io/kubehub-io/docs:main-20260731-2`. The local docker daemon is checked first; if the image is not there it is pulled from its registry. Multi-arch images are resolved to the platform of the current runner. |
| `outputImageTag`    |    no    | auto             | The remote destination image reference, e.g. `ghcr.io/kubehub-io/docs:sanitized`. Used with `publishTo: RemotePush`. See [defaulting](#outputimagetag-defaulting). |
| `publishTo`         |    no    | `RemotePush`     | Where to put the converted image: `RemotePush` (default) or `OCIArchive:<path>`. See [publishTo](#publishto).                                                                                  |
| `registryUsername`  |    no    | `github.actor`   | Username used to authenticate against the remote registry. Ignored when `publishTo: OCIArchive`.                                                                                                |
| `registryPassword`  |    no    | `github.token`   | Password/token used to authenticate against the remote registry. Ignored when `publishTo: OCIArchive`.                                                                                          |

Both `imageTag` and `outputImageTag` accept full image references, including a
tag or digest. Everything after the source is converted from the source image:
tags, manifest lists, and (if you run the converter on a node of the matching
platform) per-architecture images.

#### outputImageTag defaulting

`outputImageTag` is optional. When omitted, the converter picks a destination
automatically:

- `publishTo: RemotePush` → the remote target defaults based on where the
  converter runs:
  - **GitHub-hosted runner** (the `GITHUB_REPOSITORY` environment variable is
    present): `ghcr.io/<owner>/<repo>:sanitized`, e.g.
    `ghcr.io/kubehub-io/docs:sanitized`.
  - **On-prem** (no GitHub environment): `docker.io/<source repository
    path>:sanitized`, e.g. an `imageTag` of `my-registry.example.com/team/docs:latest`
    defaults to `docker.io/team/docs:sanitized`.
- `publishTo: OCIArchive:<path>` → `outputImageTag` is not used; the archive
  file path comes from `publishTo`.

Set `outputImageTag` explicitly whenever you want a specific tag or registry.

## Standalone usage (on-prem CI/CD)

The converter is also distributed as a standalone binary in the [GitHub
Releases](https://github.com/kubehub-io/image-volume/releases). If your CI/CD
runs on-premises and cannot use GitHub-hosted Actions, download the binary for
your platform and call it directly. It is configured through environment
variables, mirroring the Action inputs:

```bash
curl -fSL -o image-volume-converter \
  "https://github.com/kubehub-io/image-volume/releases/latest/download/image-volume-converter_linux_amd64"
chmod +x image-volume-converter

# Export the converted image as an OCI archive to a local file (no registry
# needed).
export INPUT_IMAGE_TAG="my-registry.example.com/docs:latest"
export INPUT_PUBLISH_TO="OCIArchive:/tmp/docs-sanitized.tar"

./image-volume-converter
```

To push to a remote registry instead, use `publishTo: RemotePush` (the default)
and provide a destination plus credentials:

```bash
export INPUT_IMAGE_TAG="my-registry.example.com/docs:latest"
export INPUT_PUBLISH_TO="RemotePush"
# Omit INPUT_OUTPUT_IMAGE_TAG to default to docker.io/<source repository>:sanitized.
export INPUT_OUTPUT_IMAGE_TAG="ghcr.io/kubehub-io/docs:sanitized"
# Optional: credentials. Defaults to anonymous pull / public repositories.
export INPUT_REGISTRY_USERNAME="user"
export INPUT_REGISTRY_PASSWORD="token"

./image-volume-converter
```

| Environment variable     | Required | Default      | Description                                                                                                                                 |
| ------------------------ | :------: | ------------ | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `INPUT_IMAGE_TAG`        |   yes    | —            | Source image to convert.                                                                                                                    |
| `INPUT_PUBLISH_TO`       |    no    | `RemotePush` | Where to put the converted image: `RemotePush` or `OCIArchive:<path>`.                                                                      |
| `INPUT_OUTPUT_IMAGE_TAG` |    no    | auto         | Remote destination image. Used with `RemotePush`; defaults to `docker.io/<source repository>:sanitized` on-prem. See [outputImageTag defaulting](#outputimagetag-defaulting). |
| `INPUT_REGISTRY_USERNAME` |   no    | —            | Username for remote registry authentication. Ignored with `OCIArchive`.                                                                     |
| `INPUT_REGISTRY_PASSWORD` |   no    | —            | Password/token for remote registry authentication. Ignored with `OCIArchive`.                                                               |

When pushing to a public registry repository, no credentials are needed. For
private repositories, provide credentials with push access.

> **Note:** the converted image declares `os`/`arch` as `unknown`, so it cannot
> be loaded by Docker's classic image store (`docker load` fails). containerd
> and the Kubernetes kubelet accept it — use `ctr images import` or
> `skopeo copy` to load the OCI archive into a containerd-based node.

> **Disclaimer:** The converter is developed and tested on **Linux**. Binaries
> are also built for **Windows** and **macOS**, but those platforms are **not
> tested** — `RemotePush` mode should work, while the `OCIArchive` mode is
> untested there and may not behave as expected.

## Local development

```bash
# Run the tests
go test ./...

# Build the converter
go build ./cmd/image-volume-converter
```

The repository ships a test image (`./test/Dockerfile`, a `scratch` image with
static files) used to exercise the conversion end to end.
