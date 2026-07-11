# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
FROM golang:1.25-alpine@sha256:56961d79ea8129efddcc0b8643fd8a5416b4e6228cfd477e3fd61deb2672c587 AS build

ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY internal ./internal
COPY LICENSE THIRD_PARTY_NOTICES.md ./
COPY scripts/collect_licenses.sh ./scripts/collect_licenses.sh
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /burnban .
RUN sh scripts/collect_licenses.sh /licenses
RUN mkdir -p /data && chmod 0700 /data

FROM gcr.io/distroless/static-debian12@sha256:22fd79fd75eab2372585b44517f8a094349938919dc613aafc37e4bdc9967c82

ARG VERSION=dev
ARG VCS_REF=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="Burnban" \
      org.opencontainers.image.description="Local AI-agent spend meter and budget guard" \
      org.opencontainers.image.url="https://burnban.dev" \
      org.opencontainers.image.source="https://github.com/burnban/burnban" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build --chown=65532:65532 /burnban /burnban
COPY --from=build --chown=65532:65532 /licenses /licenses
COPY --from=build --chown=65532:65532 /src/LICENSE /licenses/burnban/LICENSE
COPY --from=build --chown=65532:65532 /src/THIRD_PARTY_NOTICES.md /licenses/burnban/THIRD_PARTY_NOTICES.md
COPY --from=build --chown=65532:65532 /data /data
USER 65532:65532
VOLUME /data
ENV BURNBAN_DB=/data/burnban.db
EXPOSE 4141
ENTRYPOINT ["/burnban"]
# Non-loopback startup fails closed unless authentication and TLS (or an
# explicit reverse-proxy-only plaintext acknowledgement) are configured.
CMD ["serve", "--host", "0.0.0.0"]
