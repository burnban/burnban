FROM golang:1.25-alpine@sha256:56961d79ea8129efddcc0b8643fd8a5416b4e6228cfd477e3fd61deb2672c587 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /burnban .

FROM gcr.io/distroless/static-debian12@sha256:22fd79fd75eab2372585b44517f8a094349938919dc613aafc37e4bdc9967c82
COPY --from=build /burnban /burnban
VOLUME /data
ENV BURNBAN_DB=/data/burnban.db
EXPOSE 4141
ENTRYPOINT ["/burnban"]
# Non-loopback startup fails closed unless authentication and TLS (or an
# explicit reverse-proxy-only plaintext acknowledgement) are configured.
CMD ["serve", "--host", "0.0.0.0"]
