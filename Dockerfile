# syntax=docker/dockerfile:1
# Kabr container image: one static binary, `kabr`, on distroless (no shell,
# no package manager), running as the distroless "nonroot" user 65532.
# The same image runs `kabr operator` and `kabr serve` (the operator's
# runtime Deployments default to it through KABR_RUNTIME_IMAGE).
#
#   make docker-build              # IMG=kabr:dev, VERSION from git describe
#   docker build --build-arg VERSION=v0.3.0 -t kabr:v0.3.0 .
#
# Base images are pinned by tag AND digest (multi-arch index digests); the
# builder tag matches the toolchain line of go.mod.

FROM --platform=$BUILDPLATFORM golang:1.26.11@sha256:6ddc94d4a44aa684441991e0fcb67dfe396c4b5b446df1eb145df5f1661ea82f AS build
WORKDIR /src
# Dependencies first, so source edits do not invalidate the module layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/kabr ./cmd/kabr

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/kabr /kabr
USER 65532:65532
# `kabr serve` listens on :8080 by default.
EXPOSE 8080
ENTRYPOINT ["/kabr"]
