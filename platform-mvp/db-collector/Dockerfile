# Standalone module: build with this directory as the context.
FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/db-collector .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/db-collector /db-collector
USER nonroot:nonroot
EXPOSE 8081
ENTRYPOINT ["/db-collector"]
