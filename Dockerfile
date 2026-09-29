# Builds the console: React app first, then one static Go binary that embeds it.
FROM node:26-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-fund --no-audit
COPY web/ ./
RUN npm run build

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
COPY web/embed.go web/embed.go
COPY --from=web /src/web/dist web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ambit-console ./cmd/ambit-console \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ambit-controller ./cmd/ambit-controller

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /ambit-console /ambit-controller /
USER 65532:65532
ENTRYPOINT ["/ambit-console"]
