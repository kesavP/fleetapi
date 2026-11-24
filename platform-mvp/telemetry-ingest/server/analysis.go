package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/analysis"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
)

// Analyst writes an analysis of an incident: resolution steps, as text. The service uses analysis.Analyst (Claude); a test uses a fake.
type Analyst interface {
	Analyse(ctx context.Context, inc incident.Incident) (string, error)
}

const (
	// analysisEvery is how soon an incident may be analysed again after one was made: a click is not a loop, and each one costs money.
	analysisEvery = 2 * time.Minute
	// maxAnalyses is how many may be running at once, across all projects.
	maxAnalyses = 4
	// analysisTimeout is how long one may take: the model thinks before it answers.
	analysisTimeout = 3 * time.Minute
)

func (h *Incidents) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// begin says whether an analysis of this incident may start now; when it may not, the seconds to wait and why.
func (h *Incidents) begin(key string) (ok bool, wait int, why string) {
	h.amu.Lock()
	defer h.amu.Unlock()
	now := h.clock()
	for k, t := range h.analysed {
		if now.Sub(t) >= analysisEvery {
			delete(h.analysed, k)
		}
	}
	if t, found := h.analysed[key]; found {
		left := int((analysisEvery-now.Sub(t))/time.Second) + 1
		return false, left, "this incident was analysed (or is being) a moment ago; wait before asking again"
	}
	if h.running >= maxAnalyses {
		return false, 30, "too many analyses are running; try again in a moment"
	}
	if h.analysed == nil {
		h.analysed = map[string]time.Time{}
	}
	h.analysed[key] = now
	h.running++
	return true, 0, ""
}

// end frees the slot; a failed analysis also frees the incident, so that asking again is allowed.
func (h *Incidents) end(key string, failed bool) {
	h.amu.Lock()
	defer h.amu.Unlock()
	h.running--
	if failed {
		delete(h.analysed, key)
	}
}

// failure is what is said about an analysis that did not happen. It is a sentence of the service's own: what the API or the network said is not passed on.
func failure(err error) (status int, msg string, retry int) {
	switch {
	case errors.Is(err, analysis.ErrKeyRefused):
		return http.StatusBadGateway, "Claude could not be asked: the API refused the key set on the hub", 0
	case errors.Is(err, analysis.ErrBusy):
		return http.StatusTooManyRequests, "Claude is rate limiting this key; try again in a minute", 60
	case errors.Is(err, analysis.ErrUnavailable):
		return http.StatusBadGateway, "Claude could not be reached or had a failure; try again", 0
	case errors.Is(err, analysis.ErrDeclined):
		return http.StatusBadGateway, "Claude declined to answer for this incident", 0
	case errors.Is(err, analysis.ErrCutOff):
		return http.StatusBadGateway, "the answer was cut off before it was finished; try again", 0
	case errors.Is(err, analysis.ErrRejected):
		return http.StatusBadGateway, "the Anthropic API did not accept the request", 0
	}
	return http.StatusBadGateway, "the analysis failed", 0
}

// analyse asks the Analyst about one incident and keeps the answer as a comment by "claude". It needs a manage key, as any change does: the answer is
// written to the incident, and the call costs money.
func (h *Incidents) analyse(ctx context.Context, c call) (int, any, error) {
	if h.Analyst == nil {
		return http.StatusNotImplemented, map[string]string{"error": "analysis is not set up on this service"}, nil
	}
	id := c.r.PathValue("id")
	inc, err := h.Manager.Get(ctx, c.project, id)
	if err != nil {
		return 0, nil, err
	}
	key := c.project + "|" + id
	if ok, wait, why := h.begin(key); !ok {
		c.w.Header().Set("Retry-After", strconv.Itoa(wait))
		return http.StatusTooManyRequests, map[string]string{"error": why}, nil
	}
	failed := true
	defer func() { h.end(key, failed) }()

	// the request's own context, not the 30 seconds the rest of the API has: the model may think for a minute or two
	actx, cancel := context.WithTimeout(c.r.Context(), analysisTimeout)
	defer cancel()
	text, err := h.Analyst.Analyse(actx, inc)
	if err != nil {
		status, msg, retry := failure(err)
		h.Log.Warn("analysis failed", "project", c.project, "incident", id, "asked_by", c.actor, "error", err)
		if retry > 0 {
			c.w.Header().Set("Retry-After", strconv.Itoa(retry))
		}
		return status, map[string]string{"error": msg}, nil
	}

	// writing the comment is the API's usual quick call, and gets the usual time even when the analysis used most of the request's
	wctx, wcancel := context.WithTimeout(context.WithoutCancel(c.r.Context()), incidentTimeout)
	defer wcancel()
	updated, err := h.Manager.Comment(wctx, c.project, id, analysis.Actor, nil, text)
	if err != nil {
		return 0, nil, fmt.Errorf("keeping the analysis: %w", err)
	}
	failed = false
	h.Log.Info("analysis kept", "project", c.project, "incident", id, "asked_by", c.actor)
	return http.StatusCreated, map[string]any{"data": updated}, nil
}
