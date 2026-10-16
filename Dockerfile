# syntax=docker/dockerfile:1

# ---- build ----
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=dev COMMIT=none DATE=unknown
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
      -o /out/xai-router ./cmd/xai-router \
 && mkdir -p /out/data

# ---- runtime ----
# Distroless static: CA certificates and tzdata, no shell, runs as non-root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/xai-router /usr/local/bin/xai-router
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV XR_LISTEN=:8080 \
    XR_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/xai-router", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/xai-router"]
