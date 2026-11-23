# Test-only MCP servers (steps 5 and 9: whoami, crm; test/compressq: the answer-quality check); never shipped. Built by `make image`.
FROM golang:1.27.4@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY test/whoami test/whoami
COPY test/crm test/crm
COPY test/compressq test/compressq
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /whoami ./test/whoami \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /crm ./test/crm \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /compressq ./test/compressq
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /whoami /crm /compressq /
ENTRYPOINT ["/whoami"]
