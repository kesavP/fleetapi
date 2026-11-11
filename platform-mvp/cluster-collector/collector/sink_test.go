package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sojoner/kro-lab/platform-mvp/cluster-collector/wire"
)

const testKey = "gke-ingest-key-supersecret-0001"

func batch() wire.Batch { return wire.Batch{Version: 1, Cluster: "gke", CollectedAt: t0} }

func TestHTTPSinkPostsJSONWithTheKeyInTheHeaderOnly(t *testing.T) {
	var got wire.Batch
	var key, ct, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ct, path = r.Header.Get("X-API-Key"), r.Header.Get("Content-Type"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	s := &HTTPSink{URL: srv.URL + "/v1/inventory", Key: testKey}
	if err := s.Send(context.Background(), batch()); err != nil {
		t.Fatal(err)
	}
	if key != testKey || ct != "application/json" || path != "/v1/inventory" || got.Cluster != "gke" {
		t.Fatalf("%q %q %q %+v", key, ct, path, got)
	}
}

func TestHTTPSinkErrors(t *testing.T) {
	for code, permanent := range map[int]bool{400: true, 401: true, 403: true, 404: true, 413: true, 415: true, 408: false, 429: false, 500: false, 502: false, 503: false} {
		code := code
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"objects[0] (pod x): a pod may not carry annotations"}`))
		}))
		err := (&HTTPSink{URL: srv.URL, Key: testKey}).Send(context.Background(), batch())
		srv.Close()
		if err == nil || IsPermanent(err) != permanent {
			t.Errorf("%d: err %v, permanent %v", code, err, IsPermanent(err))
		}
		if permanent && !strings.Contains(err.Error(), "may not carry annotations") {
			t.Errorf("%d: the service's reason is lost: %v", code, err)
		}
		if err != nil && strings.Contains(err.Error(), testKey) {
			t.Errorf("%d: the key is in the error", code)
		}
	}
}

func TestHTTPSinkNeverPutsTheKeyOrTheURLInAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL + "/v1/inventory?token=" + testKey
	srv.Close() // nothing is listening
	err := (&HTTPSink{URL: url, Key: testKey}).Send(context.Background(), batch())
	if err == nil || IsPermanent(err) {
		t.Fatalf("an unreachable service is retried: %v", err)
	}
	// the host and port say where it failed, which is useful; the path and query (where a mistyped address might hold a key) are not repeated
	if strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), "token=") || strings.Contains(err.Error(), "/v1/inventory") {
		t.Fatalf("%v", err)
	}
	err = (&HTTPSink{URL: "http://exa mple.com/" + testKey, Key: testKey}).Send(context.Background(), batch())
	if !IsPermanent(err) || strings.Contains(err.Error(), testKey) {
		t.Fatalf("a bad address is permanent and does not repeat itself: %v", err)
	}
}

func TestWriterSinkPrintsOneLinePerBatch(t *testing.T) {
	var buf bytes.Buffer
	s := WriterSink{W: &buf}
	_ = s.Send(context.Background(), batch())
	_ = s.Send(context.Background(), batch())
	if lines := strings.Split(strings.TrimSpace(buf.String()), "\n"); len(lines) != 2 || !json.Valid([]byte(lines[0])) {
		t.Fatalf("%q", buf.String())
	}
}
