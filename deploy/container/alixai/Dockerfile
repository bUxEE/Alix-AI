# AlixAI service image (M5-01, master plan 16.1 and 16.2).
#
#   - multi-stage; the binary is static (CGO_ENABLED=0, pure Go SQLite) and built with -trimpath
#   - the runtime stage is distroless/static, user 65532 (nonroot): no shell, no package manager,
#     nothing to exec into. Run it with a read-only root filesystem (the stack does)
#   - the document parser worker (alixai-parser, ADR 0014) ships in the same image, next to alixai in
#     /usr/local/bin: the service starts it once per document and finds it beside itself (parse.DefaultWorkerPath),
#     so RAG file ingestion needs no configuration. It is built static, trimmed and stripped (as the service is), with NO build
#     tags: GO_TAGS applies to the service only, and the parsertest tag (fault-injection formats) must never ship.
#     Its sandbox (rlimits, Landlock, seccomp; fail closed) is installed by the worker itself and needs no capability
#     and no change to the container (its working directory is under the stack's writable /tmp); nothing in this file relaxes it
#   - both base images are pinned by digest; the tags are informational. No `# syntax=` line either: that would pull
#     an unpinned BuildKit frontend image, and nothing here needs more than the built-in one
#   - no VOLUME: data lives in bind mounts the operator names, never in an anonymous volume
#
# Build from the repository root (the context is the checkout):
#
#   docker build -f deploy/container/alixai/Dockerfile -t alixai:local .
#   docker build -f deploy/container/alixai/Dockerfile --build-arg GO_TAGS=noexternal -t alixai:local-noexternal .
#   docker build -f deploy/container/alixai/Dockerfile --target netctl -t alixai-netctl:local .
#
# Targets: `alixai` (the default, the last stage) and `netctl`, the forwarder and health probe of the stack
# without the service binary (and without the parser worker).
#
# GO_TAGS=noexternal builds the variant with no external-provider code at all: the registration file
# internal/cli/providers_external.go (//go:build !noexternal) is left out of the binary, so nothing of
# milestone X is linked, let alone initialised. The default build contains it and keeps it off until the
# operator enables External mode.

ARG GO_IMAGE=docker.io/library/golang:1.27.3-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99

# ---------------------------------------------------------------------------------------------------
FROM ${GO_IMAGE} AS build

ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local \
    GOFLAGS=-mod=readonly

WORKDIR /src

# Dependencies first: this layer is reused until go.mod or go.sum changes.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# Only the packages the binaries import travel into the context (Dockerfile.dockerignore).
COPY . .

ARG GO_TAGS=""
ARG VERSION=""
ARG COMMIT=""
ARG BUILD_DATE=""

RUN set -eu; \
    ldflags="-s -w"; \
    [ -z "$VERSION" ]    || ldflags="$ldflags -X github.com/buxee/alix-ai/internal/version.Version=$VERSION"; \
    [ -z "$COMMIT" ]     || ldflags="$ldflags -X github.com/buxee/alix-ai/internal/version.Commit=$COMMIT"; \
    [ -z "$BUILD_DATE" ] || ldflags="$ldflags -X github.com/buxee/alix-ai/internal/version.Date=$BUILD_DATE"; \
    go build -trimpath -buildvcs=false -tags "$GO_TAGS" -ldflags "$ldflags" -o /out/alixai ./cmd/alixai; \
    go build -trimpath -buildvcs=false -ldflags "-s -w" -o /out/alixai-parser ./cmd/alixai-parser; \
    go build -trimpath -buildvcs=false -ldflags "-s -w" -o /out/alixai-netctl ./deploy/container/alixai/netctl

# The two directories the service needs and distroless does not have, owned by the runtime user.
# data_dir is /var/lib/alixai (bind-mounted by the stack); the config file is mounted into /etc/alixai.
RUN mkdir -p /rootfs/var/lib/alixai /rootfs/etc/alixai && chown 65532:65532 /rootfs/var/lib/alixai

# ---------------------------------------------------------------------------------------------------
# The forwarder and health probe only: the anchor and the relay of the stack run this and nothing else.
FROM ${RUNTIME_IMAGE} AS netctl

LABEL org.opencontainers.image.title="alixai-netctl" \
      org.opencontainers.image.description="Loopback health probe and fixed-destination TCP forwarder for the AlixAI container stack" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build /out/alixai-netctl /usr/local/bin/alixai-netctl

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/alixai-netctl"]

# ---------------------------------------------------------------------------------------------------
FROM ${RUNTIME_IMAGE} AS alixai

LABEL org.opencontainers.image.title="alixai" \
      org.opencontainers.image.description="AlixAI: self-hosted, application-bound assistant service" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build --chown=65532:65532 /rootfs/var/lib/alixai /var/lib/alixai
COPY --from=build /rootfs/etc/alixai /etc/alixai
COPY --from=build /out/alixai /usr/local/bin/alixai
COPY --from=build /out/alixai-parser /usr/local/bin/alixai-parser
COPY --from=build /out/alixai-netctl /usr/local/bin/alixai-netctl

USER 65532:65532
WORKDIR /var/lib/alixai
ENTRYPOINT ["/usr/local/bin/alixai"]
CMD ["serve", "-config", "/etc/alixai/alixai.yaml"]
