# Build from the repository root. Production builds must supply immutable
# SHA-256 digests for every approved base image:
#
#   docker build \
#     --target next-production \
#     --build-arg NODE_IMAGE=node:20.19.4-alpine3.22 \
#     --build-arg NODE_IMAGE_DIGEST=<64-hex-digest> \
#     --build-arg GO_IMAGE=golang:1.25.1-alpine3.22 \
#     --build-arg GO_IMAGE_DIGEST=<64-hex-digest> \
#     --build-arg RUNTIME_IMAGE=<approved-runtime-with-git> \
#     --build-arg RUNTIME_IMAGE_DIGEST=<64-hex-digest> \
#     --build-arg VERSION=<release> --build-arg VCS_REF=$(git rev-parse HEAD) \
#     -t nexttrans .
#
# Architecture: one Go process serves the statically exported NextTrans console
# at "/" and the API/SSE/public files at /api, /sse, /files, and /translation.
# There is no Node.js or reverse proxy in the runtime image.

ARG NODE_IMAGE=node:20.19.4-alpine3.22
ARG NODE_IMAGE_DIGEST=df02558528d3d3d0d621f112e232611aecfee7cbc654f6b375765f72bb262799
ARG GO_IMAGE=golang:1.25.1-alpine3.22
ARG GO_IMAGE_DIGEST=b6ed3fd0452c0e9bcdef5597f29cc1418f61672e9d3a2f55bf02e7222c014abd
ARG RUNTIME_IMAGE=buildpack-deps:bookworm-scm
ARG RUNTIME_IMAGE_DIGEST=877e9e4d949edfbcbedabc3a2d7ab593955fee5d6d0777adf3a991eb30c750d8

# These are the approved immutable defaults for direct source builders such as
# Zeabur. CI and release jobs still pass explicit repository values and validate
# every digest before building or publishing.
FROM ${NODE_IMAGE}@sha256:${NODE_IMAGE_DIGEST} AS web-builder
WORKDIR /web
COPY web/package.json web/package-lock.json* ./
RUN npm ci --ignore-scripts --no-audit --no-fund
COPY web/ .
ENV NEXT_TELEMETRY_DISABLED=1
RUN npm run build

FROM ${GO_IMAGE}@sha256:${GO_IMAGE_DIGEST} AS go-builder
WORKDIR /src
COPY server/go.mod server/go.sum* ./
RUN go mod download && go mod verify
COPY server/ .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -buildid=" -o /moesekai-server .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -buildid= -X main.runtimeProfile=next-production" -o /moesekai-server-next-production .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -buildid=" -o /moesekai-migrate ./cmd/migrate

FROM ${RUNTIME_IMAGE}@sha256:${RUNTIME_IMAGE_DIGEST} AS runtime
ARG VERSION=dev
ARG VCS_REF=unknown
ARG IMAGE_SOURCE="https://github.com/StarMoe-org/NEXTmoetranslation"
LABEL org.opencontainers.image.title="NextTrans" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$VCS_REF \
      org.opencontainers.image.source=$IMAGE_SOURCE \
      io.nexttrans.release.mode="standalone" \
      io.nexttrans.release.workflow=".github/workflows/release-next.yml"
# The approved runtime base must already contain pinned Git, CA certificates,
# and timezone data. Mutable package installation during release builds is
# intentionally forbidden.
RUN command -v git >/dev/null && \
    test -s /etc/ssl/certs/ca-certificates.crt && \
    (test -s /usr/share/zoneinfo/UTC || test -s /usr/share/zoneinfo/Etc/UTC)
WORKDIR /app

COPY --from=go-builder /moesekai-server ./moesekai-server
COPY --from=go-builder /moesekai-migrate ./moesekai-migrate
COPY --from=web-builder /web/out ./web

# This repository intentionally ships no seed translation tree. If a separately
# reviewed seed is ever added, it must be copied explicitly and remains subject
# to the fail-closed entrypoint migration contract.
COPY --chmod=755 docker-entrypoint.sh ./docker-entrypoint.sh

# Numeric ownership avoids mutable account-file dependencies. Application code
# is root-owned and immutable to the runtime identity; only /data and the
# platform-provided temporary directory are writable.
RUN mkdir -p /data && chown -R 65532:65532 /data && chmod 0700 /data && \
    chown -R 0:0 /app && chmod -R a-w /app

ENV DB_PATH=/data/moesekai.db \
    DATA_DIR=/data \
    WEB_DIR=/app/web \
    HOME=/tmp \
    TZ=UTC \
    GOMEMLIMIT=1GiB

VOLUME ["/data"]
EXPOSE 8080

USER 65532:65532
CMD ["./docker-entrypoint.sh"]

# Standalone production is the only default final-image contract. Production
# mode and the server-owned explicit workspace-disabled mode are baked into the
# image, and no external workspace build context or files are copied.
FROM runtime AS standalone
COPY --chmod=0555 --from=go-builder /moesekai-server-next-production ./moesekai-server
ENV MOESEKAI_PRODUCTION=true \
    WORKSPACE_MODE=disabled
RUN test ! -e /app/workspace && ./moesekai-server --verify-runtime

# Keep a release-specific target name while making it the Dockerfile's default
# final stage. Local backend-only characterization may explicitly target runtime.
FROM standalone AS next-production
