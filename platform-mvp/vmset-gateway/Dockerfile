# Standalone module: build with this directory as the context.
FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/vmset-gateway .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/vmset-gateway /vmset-gateway
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/vmset-gateway"]
