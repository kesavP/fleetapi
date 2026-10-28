FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY platform-mvp/widget-operator/go.mod platform-mvp/widget-operator/go.sum platform-mvp/widget-operator/
RUN cd platform-mvp/widget-operator && go mod download
COPY platform-mvp/widget-operator platform-mvp/widget-operator
RUN cd platform-mvp/widget-operator && CGO_ENABLED=0 go build -o /out/widget-operator .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/widget-operator /widget-operator
USER nonroot:nonroot
ENTRYPOINT ["/widget-operator"]
