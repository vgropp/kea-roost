# The compiler runs natively, so that a release does not emulate other platforms.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags="-s -w -X main.version=$VERSION" -o /kea-roost ./cmd/kea-roost \
    && mkdir /data

FROM scratch
COPY --from=build /kea-roost /kea-roost
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build --chown=65532:65532 /data /data
USER 65532:65532
ENV MANAGER_DATA_DIR=/data
EXPOSE 8080
VOLUME /data
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD ["/kea-roost", "healthcheck"]
ENTRYPOINT ["/kea-roost"]
