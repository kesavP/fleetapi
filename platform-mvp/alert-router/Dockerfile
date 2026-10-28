# Standalone module with no third-party dependencies: build with this
# directory as the context.
FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -o /out/alert-router .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/alert-router /alert-router
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/alert-router"]
