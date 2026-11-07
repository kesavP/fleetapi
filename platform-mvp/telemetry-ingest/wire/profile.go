package wire

import (
	"fmt"
	"time"

	"github.com/google/pprof/profile"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/model"
)

const maxStackDepth = 512

// ProfileRows turns one pprof profile into aggregated samples. Samples with
// the same stack and sample type are summed; each distinct stack is also
// returned once, leaf frame first. A profile with several sample types
// (cpu and samples, for instance) yields one row per type.
func ProfileRows(project, service string, labels map[string]string, p *profile.Profile) ([]model.ProfileSample, []model.Stack) {
	start := time.Now().UTC()
	if p.TimeNanos != 0 {
		start = time.Unix(0, p.TimeNanos).UTC()
	}
	end := start.Add(time.Duration(p.DurationNanos))
	if labels == nil {
		labels = map[string]string{}
	}

	type key struct {
		typ  int
		hash uint64
	}
	sums := map[key]int64{}
	stacks := map[uint64][]string{}
	for _, s := range p.Sample {
		frames := stackFrames(s)
		if len(frames) == 0 {
			continue
		}
		h := hashFrames(frames)
		stacks[h] = frames
		for i, v := range s.Value {
			if v != 0 && i < len(p.SampleType) {
				sums[key{i, h}] += v
			}
		}
	}

	rows := make([]model.ProfileSample, 0, len(sums))
	for k, v := range sums {
		st := p.SampleType[k.typ]
		rows = append(rows, model.ProfileSample{
			Project: project, Service: service, Type: st.Type + ":" + st.Unit,
			Start: start, End: end, Labels: labels, Value: v, StackHash: k.hash,
		})
	}
	out := make([]model.Stack, 0, len(stacks))
	for h, f := range stacks {
		out = append(out, model.Stack{Project: project, Hash: h, Frames: f, Seen: start})
	}
	return rows, out
}

func stackFrames(s *profile.Sample) []string {
	var frames []string
	for _, loc := range s.Location {
		if len(loc.Line) == 0 {
			frames = append(frames, fmt.Sprintf("0x%x", loc.Address))
		}
		// Line[0] is the innermost inlined call, so leaf-first order holds.
		for _, ln := range loc.Line {
			name := "unknown"
			if ln.Function != nil && ln.Function.Name != "" {
				name = ln.Function.Name
			}
			frames = append(frames, name)
		}
		if len(frames) >= maxStackDepth {
			return frames[:maxStackDepth]
		}
	}
	return frames
}

func hashFrames(frames []string) uint64 {
	h := uint64(fnvOffset)
	for _, f := range frames {
		h = fnvString(h, f)
		h ^= 0xff
		h *= fnvPrime
	}
	return h
}
