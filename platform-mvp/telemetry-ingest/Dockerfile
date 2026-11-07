# Standalone module: build with this directory as the context.
FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/telemetry-ingest .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/telemetry-ingest /telemetry-ingest
USER nonroot:nonroot
EXPOSE 8080 4317
ENTRYPOINT ["/telemetry-ingest"]
