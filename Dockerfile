# Base images are pinned by digest; Dependabot proposes updates.
# Builds the console: React app first, then one static Go binary that embeds it.
FROM --platform=$BUILDPLATFORM node:26-alpine@sha256:6f08a523443ddedf52941874c203475b0ad503e859ad70cb26ab8be9a517dbd5 AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-fund --no-audit
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27.2@sha256:820153096fda4de6ac1b7194d12e416b7dd8f4e9a33d17e0345b111b945162d8 AS build
WORKDIR /src
COPY go.mod go.sum ./
# Cache mounts keep Go's module and build caches out of image layers: rebuilds reuse one cache
# instead of adding a gigabyte of layers each time.
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
COPY deploy/ deploy/
COPY kind/ kind/
COPY web/embed.go web/embed.go
COPY --from=web /src/web/dist web/dist
ARG TARGETOS TARGETARCH
# Cross-compile on the build machine instead of emulating the target architecture.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ambit-console ./cmd/ambit-console \
 && GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ambit-controller ./cmd/ambit-controller \
 && GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ambit ./cmd/ambit \
 && GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ambit-guard ./cmd/ambit-guard \
 && GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ambit-router ./cmd/ambit-router

# CI: the build job compiles the binaries with Go's cache warm (setup-go) into bin/ and builds this target, so the
# e2e jobs do not compile again. Same base and layout as the default image below.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3 AS prebuilt
COPY bin/ambit-console bin/ambit-controller bin/ambit bin/ambit-guard bin/ambit-router /
USER 65532:65532
ENTRYPOINT ["/ambit-console"]

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /ambit-console /ambit-controller /ambit /ambit-guard /ambit-router /
USER 65532:65532
ENTRYPOINT ["/ambit-console"]
