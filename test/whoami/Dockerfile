# Test-only MCP servers (steps 5 and 9: whoami, crm); never shipped. Built by `make image`.
FROM golang:1.27.2@sha256:820153096fda4de6ac1b7194d12e416b7dd8f4e9a33d17e0345b111b945162d8 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY test/whoami test/whoami
COPY test/crm test/crm
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /whoami ./test/whoami \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /crm ./test/crm
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /whoami /crm /
ENTRYPOINT ["/whoami"]
