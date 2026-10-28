FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY platform-mvp/oidc-verifier/go.mod platform-mvp/oidc-verifier/go.sum platform-mvp/oidc-verifier/
RUN cd platform-mvp/oidc-verifier && go mod download
COPY platform-mvp/oidc-verifier platform-mvp/oidc-verifier
RUN cd platform-mvp/oidc-verifier && CGO_ENABLED=0 go build -o /out/oidc-verifier .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/oidc-verifier /oidc-verifier
USER nonroot:nonroot
ENTRYPOINT ["/oidc-verifier"]