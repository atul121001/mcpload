# syntax=docker/dockerfile:1
#
# mcpload in a container: the mcpload CLI, its load engine (a k6 build with
# xk6-mcpload) and the bundled scenarios. No clone or toolchain needed:
#
#   docker run --rm -v "$PWD:/work" ghcr.io/atul121001/mcpload run --url http://host.docker.internal:3001/mcp
#
# Reports are written to /work (the working directory), so mount a host folder
# there. On Linux add --add-host=host.docker.internal:host-gateway to reach a
# server on the host.
#
# Build: docker build -t mcpload:dev .   (add --build-arg VERSION=v0.3.0 to stamp a version)

ARG GO_VERSION=1.26
# Same versions as .github/workflows/release.yml. (Not K6_VERSION: xk6 reads that env var too.)
ARG MCPLOAD_K6_VERSION=v2.3.0
ARG XK6_VERSION=v1.4.14

# The builder runs on the build machine's platform and cross-compiles, so
# multi-arch builds don't run the Go toolchain under emulation.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG MCPLOAD_K6_VERSION
ARG XK6_VERSION
ARG VERSION=""
ARG TARGETOS
ARG TARGETARCH
RUN apk add --no-cache git
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go install go.k6.io/xk6@"$XK6_VERSION"
WORKDIR /src
RUN mkdir -p /out
COPY xk6-mcpload/ xk6-mcpload/
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOOS="$TARGETOS" GOARCH="$TARGETARCH" CGO_ENABLED=0 \
    xk6 build "$MCPLOAD_K6_VERSION" \
      --with github.com/atul121001/mcpload/xk6-mcpload=./xk6-mcpload \
      --output /out/k6
COPY cmd/mcpload/ cmd/mcpload/
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    cd cmd/mcpload && \
    ldflags="-s -w"; [ -n "$VERSION" ] && ldflags="$ldflags -X main.version=$VERSION"; \
    GOOS="$TARGETOS" GOARCH="$TARGETARCH" CGO_ENABLED=0 \
    go build -trimpath -ldflags "$ldflags" -o /out/mcpload .
COPY scenarios/ /out/scenarios/
RUN rm -f /out/scenarios/package.json /out/scenarios/lib/*.test.mjs && \
    mkdir -p /out-bin && ln -s /opt/mcpload/mcpload /out-bin/mcpload

# Alpine: small, ships CA certificates (ca-certificates-bundle) for https
# targets, and has a shell for debugging (--entrypoint sh).
FROM alpine:3.22
# Everything lives in /opt/mcpload; mcpload finds the engine and the
# scenarios next to its own executable, so `--scenario soak` works by name.
COPY --from=build /out/ /opt/mcpload/
# (No RUN here, so multi-arch builds need no emulation for this stage.)
COPY --from=build /out-bin/ /usr/local/bin/
# This mcpload is built without the embedded engine, so it would otherwise look
# for ./k6 in the working directory first, which is the user's mounted folder:
# always run the engine shipped in the image.
ENV MCPLOAD_ENGINE=/opt/mcpload/k6
WORKDIR /work
ENTRYPOINT ["mcpload"]
CMD ["--help"]
