# syntax=docker/dockerfile:1

# Toolchain versions available to hosted projects. Bump them here.
ARG GO_VERSION=1.27
ARG NODE_VERSION=24

# Build stages run on the builder's own platform (no CPU emulation when building
# for another one); only the runtime stage uses the target platform.

# --- Build the admin web app (static files, platform-independent) -------------
FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-trixie-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-fund --no-audit
COPY web/ ./
# Outputs to /src/internal/api/ui/dist (see web/vite.config.ts).
RUN npm run build

# --- Build the self-hoster binary (embeds the web app), cross-compiled ----------
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-trixie AS build
ARG TARGETOS TARGETARCH
# Version shown on the settings page, e.g. 2.42 (set by CI; the git commit or "dev" when empty).
ARG VERSION=
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/api/ui/dist ./internal/api/ui/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X selfhoster/internal/buildinfo.version=${VERSION}" -o /out/self-hoster ./cmd/self-hoster

# Go toolchain for hosted Go projects: must match the target platform.
FROM golang:${GO_VERSION}-trixie AS gotoolchain

# --- Runtime: Node + Go toolchains, git, and the supervisor --------------------
FROM node:${NODE_VERSION}-trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates openssh-client tini \
 && rm -rf /var/lib/apt/lists/*
COPY --from=gotoolchain /usr/local/go /usr/local/go

# Build caches live on the data volume, so rebuilds stay fast across image upgrades.
ENV PATH=/usr/local/go/bin:/data/cache/go/bin:$PATH \
    GOPATH=/data/cache/go \
    GOCACHE=/data/cache/go-build \
    npm_config_cache=/data/cache/npm \
    SELFHOSTER_DATA_DIR=/data \
    SELFHOSTER_APPS_DIR=/apps

COPY --from=build /out/self-hoster /usr/local/bin/self-hoster
# /data: self-hoster state, checkouts, caches. /apps: each project's persistent data (DATA_DIR).
VOLUME /data /apps
EXPOSE 8080
# tini reaps orphaned grandchildren and forwards signals to the supervisor.
ENTRYPOINT ["tini", "--", "self-hoster"]
