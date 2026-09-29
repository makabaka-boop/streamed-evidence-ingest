# Build the static service binaries. Pure-Go SQLite (modernc.org/sqlite)
# means no cgo toolchain and no libc are needed at build or run time.
FROM golang:1.23-bookworm AS build
WORKDIR /src

# Cache dependencies independently of source changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w" -o /out/uploadsrv ./cmd/uploadsrv

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w" -o /out/verify ./cmd/verify

# A writable data dir owned by the nonroot uid (65532). When Docker creates
# the named volume at /data it inherits this ownership, so the unprivileged
# process can create its database and blob/tmp directories.
RUN mkdir -p /data && chown 65532:65532 /data

# Runtime image: static binaries, no shell, single non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/uploadsrv /usr/local/bin/uploadsrv
COPY --from=build /out/verify    /usr/local/bin/verify
COPY --from=build --chown=65532:65532 /data /data
USER nonroot:nonroot
ENV UPLOAD_DATA_DIR=/data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/uploadsrv"]
