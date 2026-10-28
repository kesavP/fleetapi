# Build context must be the repo root: go.mod replaces github.com/sojoner/kro-lab
# with the local checkout (the cluster-inventory-api provider lives there).
FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
COPY platform-mvp/vm-binding-controller/go.mod platform-mvp/vm-binding-controller/go.sum platform-mvp/vm-binding-controller/
RUN cd platform-mvp/vm-binding-controller && go mod download
COPY . .
RUN cd platform-mvp/vm-binding-controller && CGO_ENABLED=0 go build -o /out/vm-binding-controller .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/vm-binding-controller /vm-binding-controller
USER nonroot:nonroot
ENTRYPOINT ["/vm-binding-controller"]
