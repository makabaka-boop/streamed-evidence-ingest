# syntax=docker/dockerfile:1

# ---- build stage: compile the pure-Go binaries and run the unit suite ----
FROM golang:1.23-bookworm AS build
WORKDIR /src

# Resolve dependencies first for better layer caching.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Fail the image build if the package tests fail.
RUN CGO_ENABLED=0 go test ./... && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
        -ldflags="-s -w" -o /out/server ./cmd/server && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
        -ldflags="-s -w" -o /out/verify ./cmd/verify

# ---- runtime stage: minimal image containing both commands ----
FROM debian:bookworm-slim AS runtime
# ca-certificates for completeness; the service itself makes no outbound TLS.
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/* && \
    groupadd --system upload && useradd --system --gid upload --home-dir /data upload

COPY --from=build /out/server /usr/local/bin/server
COPY --from=build /out/verify /usr/local/bin/verify

RUN mkdir -p /data /faultdata && chown -R upload:upload /data /faultdata
USER upload
WORKDIR /

# The server is the default command; compose overrides it for the verify
# one-shot container.
EXPOSE 8080
ENTRYPOINT []
CMD ["/usr/local/bin/server", "-addr", ":8080", "-data", "/data"]
