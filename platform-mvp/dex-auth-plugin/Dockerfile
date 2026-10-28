FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY platform-mvp/dex-auth-plugin/go.mod platform-mvp/dex-auth-plugin/go.sum* ./
RUN go mod download
COPY platform-mvp/dex-auth-plugin/ .
RUN CGO_ENABLED=0 go build -o /dex-auth-plugin .

FROM alpine:3.21
COPY --from=builder /dex-auth-plugin /usr/local/bin/dex-auth-plugin
ENTRYPOINT ["/usr/local/bin/dex-auth-plugin"]