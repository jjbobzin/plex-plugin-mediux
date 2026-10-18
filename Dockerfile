FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/plex-plugin-mediux .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/plex-plugin-mediux /plex-plugin-mediux
EXPOSE 32600
VOLUME /config
ENTRYPOINT ["/plex-plugin-mediux"]
