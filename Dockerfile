FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /burnban .

FROM gcr.io/distroless/static-debian12
COPY --from=build /burnban /burnban
VOLUME /data
ENV BURNBAN_DB=/data/burnban.db
EXPOSE 4141
ENTRYPOINT ["/burnban"]
# Team mode binds beyond loopback, so it fails closed unless BURNBAN_TOKEN
# is provided at runtime: docker run -e BURNBAN_TOKEN=... -p 4141:4141 burnban
CMD ["serve", "--host", "0.0.0.0"]
