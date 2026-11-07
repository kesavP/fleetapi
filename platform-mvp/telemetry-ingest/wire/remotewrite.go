package wire

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/golang/snappy"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/model"
	"google.golang.org/protobuf/encoding/protowire"
)

// The Prometheus remote-write 1.0 message is small and stable, so it is
// decoded straight from the wire format instead of pulling in the Prometheus
// module and its dependency tree.
//
//	WriteRequest   { repeated TimeSeries timeseries = 1; repeated MetricMetadata metadata = 3; }
//	TimeSeries     { repeated Label labels = 1; repeated Sample samples = 2; }
//	Label          { string name = 1; string value = 2; }
//	Sample         { double value = 1; int64 timestamp = 2; }
//	MetricMetadata { enum type = 1; string metric_family_name = 2; string help = 4; string unit = 5; }

const (
	maxLabelsPerSeries = 256
	metricNameLabel    = "__name__"
)

type Sample struct {
	TimestampMs int64
	Value       float64
}

type TimeSeries struct {
	Labels  []Label
	Samples []Sample
}

type Metadata struct{ Type, Family, Help, Unit string }

type RemoteWrite struct {
	Series   []TimeSeries
	Metadata []Metadata
}

var metricTypes = []string{"UNKNOWN", "COUNTER", "GAUGE", "SUMMARY", "HISTOGRAM", "GAUGEHISTOGRAM", "INFO", "STATESET"}

// DecodeRemoteWrite expects the snappy block format that remote-write uses.
func DecodeRemoteWrite(compressed []byte, max int64) (*RemoteWrite, error) {
	n, err := snappy.DecodedLen(compressed)
	if err != nil {
		return nil, fmt.Errorf("snappy: %w", err)
	}
	if int64(n) > max {
		return nil, ErrTooLarge
	}
	raw, err := snappy.Decode(nil, compressed)
	if err != nil {
		return nil, fmt.Errorf("snappy: %w", err)
	}
	return ParseRemoteWrite(raw)
}

func ParseRemoteWrite(b []byte) (*RemoteWrite, error) {
	rw := &RemoteWrite{}
	err := eachField(b, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		if typ != protowire.BytesType {
			return nil
		}
		switch num {
		case 1:
			ts, err := parseTimeSeries(v)
			if err != nil {
				return err
			}
			rw.Series = append(rw.Series, ts)
		case 3:
			md, err := parseMetadata(v)
			if err != nil {
				return err
			}
			rw.Metadata = append(rw.Metadata, md)
		}
		return nil
	})
	return rw, err
}

// eachField walks one message. For bytes fields v is the payload; for
// varint and fixed fields u is the value. Unknown fields are skipped.
func eachField(b []byte, fn func(num protowire.Number, typ protowire.Type, v []byte, u uint64) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		var v []byte
		var u uint64
		switch typ {
		case protowire.BytesType:
			v, n = protowire.ConsumeBytes(b)
		case protowire.VarintType:
			u, n = protowire.ConsumeVarint(b)
		case protowire.Fixed64Type:
			u, n = protowire.ConsumeFixed64(b)
		default:
			n = protowire.ConsumeFieldValue(num, typ, b)
		}
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		if err := fn(num, typ, v, u); err != nil {
			return err
		}
	}
	return nil
}

func parseTimeSeries(b []byte) (TimeSeries, error) {
	var ts TimeSeries
	err := eachField(b, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		if typ != protowire.BytesType {
			return nil
		}
		switch num {
		case 1:
			if len(ts.Labels) >= maxLabelsPerSeries {
				return fmt.Errorf("series has more than %d labels", maxLabelsPerSeries)
			}
			var l Label
			err := eachField(v, func(num protowire.Number, typ protowire.Type, s []byte, _ uint64) error {
				if typ != protowire.BytesType {
					return nil
				}
				switch num {
				case 1:
					l.Name = string(s)
				case 2:
					l.Value = string(s)
				}
				return nil
			})
			if err != nil {
				return err
			}
			ts.Labels = append(ts.Labels, l)
		case 2:
			var s Sample
			err := eachField(v, func(num protowire.Number, typ protowire.Type, _ []byte, u uint64) error {
				switch {
				case num == 1 && typ == protowire.Fixed64Type:
					s.Value = math.Float64frombits(u)
				case num == 2 && typ == protowire.VarintType:
					s.TimestampMs = int64(u)
				}
				return nil
			})
			if err != nil {
				return err
			}
			ts.Samples = append(ts.Samples, s)
		}
		return nil
	})
	return ts, err
}

func parseMetadata(b []byte) (Metadata, error) {
	var md Metadata
	err := eachField(b, func(num protowire.Number, typ protowire.Type, v []byte, u uint64) error {
		switch {
		case num == 1 && typ == protowire.VarintType:
			md.Type = "UNKNOWN"
			if u < uint64(len(metricTypes)) {
				md.Type = metricTypes[u]
			}
		case num == 2 && typ == protowire.BytesType:
			md.Family = string(v)
		case num == 4 && typ == protowire.BytesType:
			md.Help = string(v)
		case num == 5 && typ == protowire.BytesType:
			md.Unit = string(v)
		}
		return nil
	})
	if md.Type == "" {
		md.Type = "UNKNOWN"
	}
	return md, err
}

var errNoMetricName = errors.New("series without __name__")

// MetricRows flattens a decoded request. Series without a __name__ label or
// without samples carry nothing to store and are skipped. Sample times keep
// their millisecond precision.
func MetricRows(project string, rw *RemoteWrite) ([]model.Sample, []model.Series, []model.MetricMeta) {
	var samples []model.Sample
	series := make([]model.Series, 0, len(rw.Series))
	now := time.Now().UTC()
	for _, ts := range rw.Series {
		if len(ts.Samples) == 0 {
			continue
		}
		sorted := append([]Label(nil), ts.Labels...)
		sortLabels(sorted)
		name, labels, err := splitName(sorted)
		if err != nil {
			continue
		}
		hash := HashLabels(sorted)
		series = append(series, model.Series{Project: project, Name: name, Hash: hash, Labels: labels, Seen: now})
		for _, s := range ts.Samples {
			samples = append(samples, model.Sample{
				Project: project, Name: name, Hash: hash,
				Time: time.UnixMilli(s.TimestampMs).UTC(), Value: s.Value,
			})
		}
	}
	meta := make([]model.MetricMeta, 0, len(rw.Metadata))
	for _, m := range rw.Metadata {
		if m.Family == "" {
			continue
		}
		meta = append(meta, model.MetricMeta{Project: project, Family: m.Family, Type: m.Type, Help: m.Help, Unit: m.Unit})
	}
	return samples, series, meta
}

func splitName(sorted []Label) (string, map[string]string, error) {
	labels := make(map[string]string, len(sorted))
	var name string
	for _, l := range sorted {
		if l.Name == metricNameLabel {
			name = l.Value
			continue
		}
		labels[l.Name] = l.Value
	}
	if name == "" {
		return "", nil, errNoMetricName
	}
	return name, labels, nil
}
