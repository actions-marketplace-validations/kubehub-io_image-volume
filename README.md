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
so that `architecture` and `os` are `unknown`, and either loads the result back
into the local docker daemon or pushes it to ghcr.io. It works the same way
whether you use it as a GitHub Action or as a standalone binary:

1. Load the source image — from the local docker daemon if available, otherwise
   by pulling it from its registry for the platform of the current machine.
2. Export the image as an OCI archive and unpack it into an OCI layout.
3. Resolve `index.json → manifest → config`, set `architecture` and `os` to
   `unknown`, and repack a fresh layout.
4. Load the converted image into the local docker daemon, or push it to the
   destination repository on ghcr.io.

Only the image config is rewritten; the layer blobs are reused as-is.

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
          targetImageTag: ghcr.io/${{ github.repository }}-test:sanitized
```

### publishTo

`publishTo` controls where the converted image goes:

| Value          | Behavior                                                                                                       |
| -------------- | -------------------------------------------------------------------------------------------------------------- |
| `RemotePush`   | Push the converted image to a remote registry (`targetImageTag`). **Default.**                                |
| `DockerDaemon` | Load the converted image into the local docker daemon instead, e.g. to inspect it or export it for on-prem use. |

To keep the converted image on the runner instead of pushing it to ghcr.io, set
`publishTo: DockerDaemon`. The image is loaded into the local docker daemon
under `targetImageTag` (or `imageTag` if omitted):

```yaml
      - name: Sanitize and load into local docker daemon
        uses: kubehub-io/image-volume@v1
        with:
          imageTag: ghcr.io/${{ github.repository }}-test:main-latest
          targetImageTag: ghcr.io/${{ github.repository }}-test:sanitized
          publishTo: DockerDaemon
```

### Inputs

| Input               | Required | Default          | Description                                                                                                                                                                                     |
| ------------------- | :------: | ---------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `imageTag`          |   yes    | —                | The source image to convert, e.g. `ghcr.io/kubehub-io/docs:main-20260731-2`. The local docker daemon is checked first; if the image is not there it is pulled from its registry. Multi-arch images are resolved to the platform of the current runner. |
| `targetImageTag`    |    no    | auto             | The destination image reference, e.g. `ghcr.io/kubehub-io/docs:sanitized`. With `publishTo: DockerDaemon` this is the tag the image is loaded under in the local docker daemon. With `publishTo: RemotePush` it is the remote target. See [defaulting](#targetimagetag-defaulting). |
| `publishTo`         |    no    | `RemotePush`     | Where to put the converted image: `RemotePush` (default) or `DockerDaemon`. See [publishTo](#publishto).                                                                                       |
| `registryUsername`  |    no    | `github.actor`   | Username used to authenticate against the remote registry. Ignored when `publishTo: DockerDaemon`.                                                                                              |
| `registryPassword`  |    no    | `github.token`   | Password/token used to authenticate against the remote registry. Ignored when `publishTo: DockerDaemon`.                                                                                         |

Both `imageTag` and `targetImageTag` accept full image references, including a
tag or digest. Everything after the source is converted from the source image:
tags, manifest lists, and (if you run the converter on a node of the matching
platform) per-architecture images.

#### targetImageTag defaulting

`targetImageTag` is optional. When omitted, the converter picks a destination
automatically:

- `publishTo: DockerDaemon` → the local tag defaults to `imageTag`.
- `publishTo: RemotePush` → the remote target defaults based on where the
  converter runs:
  - **GitHub-hosted runner** (the `GITHUB_REPOSITORY` environment variable is
    present): `ghcr.io/<owner>/<repo>:sanitized`, e.g.
    `ghcr.io/kubehub-io/docs:sanitized`.
  - **On-prem** (no GitHub environment): `docker.io/<source repository
    path>:sanitized`, e.g. an `imageTag` of `my-registry.example.com/team/docs:latest`
    defaults to `docker.io/team/docs:sanitized`.

Set `targetImageTag` explicitly whenever you want a specific tag or registry.

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

# Load the converted image into the local docker daemon (no registry needed).
export INPUT_IMAGE_TAG="my-registry.example.com/docs:latest"
export INPUT_PUBLISH_TO="DockerDaemon"
# The tag the image is loaded under in the daemon; defaults to INPUT_IMAGE_TAG.
export INPUT_TARGET_IMAGE_TAG="my-registry.example.com/docs:sanitized"

./image-volume-converter
```

To push to a remote registry instead, use `publishTo: RemotePush` (the default)
and provide a destination plus credentials:

```bash
export INPUT_IMAGE_TAG="my-registry.example.com/docs:latest"
export INPUT_PUBLISH_TO="RemotePush"
# Omit INPUT_TARGET_IMAGE_TAG to default to docker.io/<source repository>:sanitized.
export INPUT_TARGET_IMAGE_TAG="ghcr.io/kubehub-io/docs:sanitized"
# Optional: credentials. Defaults to anonymous pull / public repositories.
export INPUT_REGISTRY_USERNAME="user"
export INPUT_REGISTRY_PASSWORD="token"

./image-volume-converter
```

| Environment variable   | Required | Default    | Description                                                                                                                              |
| ---------------------- | :------: | ---------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `INPUT_IMAGE_TAG`      |   yes    | —          | Source image to convert.                                                                                                                 |
| `INPUT_PUBLISH_TO`     |    no    | `RemotePush` | Where to put the converted image: `RemotePush` or `DockerDaemon`.                                                                      |
| `INPUT_TARGET_IMAGE_TAG` |   no    | auto       | Destination image. With `DockerDaemon` it is the local tag to load under (defaults to `INPUT_IMAGE_TAG`); with `RemotePush` it is the remote target. See [targetImageTag defaulting](#targetimagetag-defaulting). |
| `INPUT_REGISTRY_USERNAME` |   no    | —          | Username for remote registry authentication. Ignored with `DockerDaemon`.                                                               |
| `INPUT_REGISTRY_PASSWORD` |   no    | —          | Password/token for remote registry authentication. Ignored with `DockerDaemon`.                                                          |

When pushing to a public registry repository, no credentials are needed. For
private repositories, provide credentials with push access.

## Local development

```bash
# Run the tests
go test ./...

# Build the converter
go build ./cmd/image-volume-converter
```

The repository ships a test image (`./test/Dockerfile`, a `scratch` image with
static files) used to exercise the conversion end to end.
