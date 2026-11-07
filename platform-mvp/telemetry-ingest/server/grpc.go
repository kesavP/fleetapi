package server

import (
	"context"
	"errors"
	"strings"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/pipeline"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/wire"
	logsv1 "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	tracesv1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// NewGRPC serves OTLP/gRPC for traces and logs, for applications that export
// with an OpenTelemetry SDK. Metrics and profiles are HTTP only.
func NewGRPC(res Resolver, sink Sink, maxBody int, count *Counters) *grpc.Server {
	g := &grpcIngest{res: res, sink: sink, count: count}
	srv := grpc.NewServer(grpc.MaxRecvMsgSize(maxBody))
	tracesv1.RegisterTraceServiceServer(srv, &grpcTraces{grpcIngest: g})
	logsv1.RegisterLogsServiceServer(srv, &grpcLogs{grpcIngest: g})
	return srv
}

type grpcIngest struct {
	res   Resolver
	sink  Sink
	count *Counters
}

func (g *grpcIngest) project(ctx context.Context) (string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	key := ""
	if v := md.Get("x-api-key"); len(v) > 0 {
		key = v[0]
	} else if v := md.Get("authorization"); len(v) > 0 && len(v[0]) > 7 && strings.EqualFold(v[0][:7], "bearer ") {
		key = strings.TrimSpace(v[0][7:])
	}
	p, ok := g.res.IngestProject(key)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "missing or invalid API key")
	}
	return p, nil
}

func (g *grpcIngest) result(signal string, err error) error {
	switch {
	case err == nil:
		g.count.Inc("telemetry_ingest_requests_total", "signal", signal, "code", "OK")
		return nil
	case errors.Is(err, pipeline.ErrOverloaded):
		g.count.Inc("telemetry_ingest_requests_total", "signal", signal, "code", "ResourceExhausted")
		return status.Error(codes.ResourceExhausted, "storage is behind, retry later")
	}
	g.count.Inc("telemetry_ingest_requests_total", "signal", signal, "code", "Internal")
	return status.Error(codes.Internal, "internal error")
}

type grpcTraces struct {
	*grpcIngest
	tracesv1.UnimplementedTraceServiceServer
}

func (g *grpcTraces) Export(ctx context.Context, req *tracesv1.ExportTraceServiceRequest) (*tracesv1.ExportTraceServiceResponse, error) {
	project, err := g.project(ctx)
	if err != nil {
		g.count.Inc("telemetry_ingest_requests_total", "signal", "traces", "code", "Unauthenticated")
		return nil, err
	}
	if err := g.result("traces", g.sink.AddSpans(wire.Spans(project, req))); err != nil {
		return nil, err
	}
	return &tracesv1.ExportTraceServiceResponse{}, nil
}

type grpcLogs struct {
	*grpcIngest
	logsv1.UnimplementedLogsServiceServer
}

func (g *grpcLogs) Export(ctx context.Context, req *logsv1.ExportLogsServiceRequest) (*logsv1.ExportLogsServiceResponse, error) {
	project, err := g.project(ctx)
	if err != nil {
		g.count.Inc("telemetry_ingest_requests_total", "signal", "logs", "code", "Unauthenticated")
		return nil, err
	}
	if err := g.result("logs", g.sink.AddLogs(wire.Logs(project, req))); err != nil {
		return nil, err
	}
	return &logsv1.ExportLogsServiceResponse{}, nil
}
