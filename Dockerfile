# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/drivetemp-sensor ./cmd/drivetemp-sensor \
 && mkdir -p /out/run/drivetemp-sensor

FROM scratch
COPY --from=build /out/drivetemp-sensor /drivetemp-sensor
# Default output directory, owned by the runtime user so the image also works
# without a volume; a new named volume mounted here inherits the ownership.
COPY --from=build --chown=65534:65534 /out/run/drivetemp-sensor /run/drivetemp-sensor
USER 65534:65534
ENTRYPOINT ["/drivetemp-sensor"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD ["/drivetemp-sensor", "healthcheck"]
