# syntax=docker/dockerfile:1.7
#
# Targets:
#   standalone (default) — iag-federation-gateway repo root on Railway
#   monorepo             — IAG_multi_backend root context (deploy/docker-compose)
#
# Monorepo:   docker build -f edge/federation-gateway/Dockerfile --target monorepo .
# Standalone: docker build --target standalone .

FROM golang:1.25-alpine AS base
RUN apk add --no-cache git ca-certificates
ENV PLATFORM_GO_DEP=/deps/platform-go

FROM base AS platform-go-copy
COPY shared/platform-go ${PLATFORM_GO_DEP}

FROM base AS build-standalone
# Standalone (iag-federation-gateway repo root): the meta-repo is private, so
# Railway cannot clone it at build time. The standalone repo carries a committed
# snapshot at third_party/platform-go (refreshed via scripts/sync-platform-go.sh).
WORKDIR /src
COPY third_party/platform-go ${PLATFORM_GO_DEP}
COPY go.mod go.sum ./
RUN go mod edit -replace=github.com/alvor-technologies/iag-platform-go=${PLATFORM_GO_DEP} \
    && go mod download
COPY . .
ARG VERSION=dev
# `COPY . .` restored go.mod from the build context, which still carries the
# meta-repo-only replace path. Re-apply the vendored replace before build.
RUN go mod edit -replace=github.com/alvor-technologies/iag-platform-go=${PLATFORM_GO_DEP} \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /federation-gateway .

FROM base AS build-monorepo
COPY --from=platform-go-copy ${PLATFORM_GO_DEP} ${PLATFORM_GO_DEP}
WORKDIR /src/edge/federation-gateway
COPY edge/federation-gateway/go.mod edge/federation-gateway/go.sum ./
RUN go mod edit -replace=github.com/alvor-technologies/iag-platform-go=${PLATFORM_GO_DEP} \
    && go mod download
COPY edge/federation-gateway/ .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /federation-gateway .

FROM alpine:3.21 AS monorepo
RUN apk add --no-cache ca-certificates tzdata wget
WORKDIR /app
COPY --from=build-monorepo /federation-gateway /app/federation-gateway
ENV PORT=4021 \
    GIN_MODE=release \
    LOG_FORMAT=json \
    AUTO_MIGRATE=false
EXPOSE 4021
HEALTHCHECK --interval=15s --timeout=5s --start-period=25s --retries=5 \
  CMD wget -q -O /dev/null http://127.0.0.1:4021/ready || exit 1
USER nobody
ENTRYPOINT ["/app/federation-gateway"]

FROM alpine:3.21 AS standalone
RUN apk add --no-cache ca-certificates tzdata wget
WORKDIR /app
COPY --from=build-standalone /federation-gateway /app/federation-gateway
ENV PORT=4021 \
    GIN_MODE=release \
    LOG_FORMAT=json \
    AUTO_MIGRATE=false
EXPOSE 4021
HEALTHCHECK --interval=15s --timeout=5s --start-period=25s --retries=5 \
  CMD wget -q -O /dev/null http://127.0.0.1:4021/ready || exit 1
USER nobody
ENTRYPOINT ["/app/federation-gateway"]
