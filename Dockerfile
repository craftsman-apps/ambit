# Base images are pinned by digest; Dependabot proposes updates.
# Builds the console: React app first, then one static Go binary that embeds it.
FROM --platform=$BUILDPLATFORM node:26-alpine@sha256:c6ad9e9c1d80f6eb607c26b4b0a132ff7a2344fad1c78ea3baa2a7d4c8de6d94 AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-fund --no-audit
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27.3@sha256:e519d3b0a5b285e5aa53ab9eef02bc6bbb9cb04608389b743a136a9ce75f0d1e AS build
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
FROM gcr.io/distroless/static-debian13:nonroot@sha256:516757106de35205efb70b91ba03f6f1298424a0678c603255cf2852a7000087 AS prebuilt
COPY bin/ambit-console bin/ambit-controller bin/ambit bin/ambit-guard bin/ambit-router /
USER 65532:65532
ENTRYPOINT ["/ambit-console"]

FROM gcr.io/distroless/static-debian13:nonroot@sha256:516757106de35205efb70b91ba03f6f1298424a0678c603255cf2852a7000087
COPY --from=build /ambit-console /ambit-controller /ambit /ambit-guard /ambit-router /
USER 65532:65532
ENTRYPOINT ["/ambit-console"]
