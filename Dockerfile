# Build a static binary (modernc.org/sqlite is pure Go, no cgo needed).
FROM golang:1.26-alpine AS build
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /pageforge .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /pageforge /pageforge
VOLUME /data
ENV PAGEFORGE_DB=/data/pageforge.db PAGEFORGE_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/pageforge"]
