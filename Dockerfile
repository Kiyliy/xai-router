# syntax=docker/dockerfile:1

# ---- build ----
# Base images are pinned by digest; bump them deliberately (docker buildx imagetools inspect <image>).
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d AS build
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
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/xai-router /usr/local/bin/xai-router
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV XR_LISTEN=:8080 \
    XR_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/xai-router", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/xai-router"]
