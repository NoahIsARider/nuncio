# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o /out/nuncio ./cmd/nuncio

FROM alpine:3.20
RUN apk add --no-cache git openssh-client ca-certificates tzdata \
    && adduser -D -h /var/lib/nuncio -s /sbin/nologin nuncio
COPY --from=build /out/nuncio /usr/local/bin/nuncio
USER nuncio
WORKDIR /var/lib/nuncio
VOLUME ["/var/lib/nuncio"]
EXPOSE 8787
ENTRYPOINT ["nuncio"]
CMD ["serve", "-c", "/etc/nuncio/nuncio.toml"]
