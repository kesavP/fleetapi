package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/vmset-gateway/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

const goodKey = "ssh-ed25519 AAAAC3Nza me@host"

var testGVR = schema.GroupVersionResource{Group: model.Group, Version: model.Version, Resource: "vmsets"}

type harness struct {
	srv    *httptest.Server
	dc     *dynamicfake.FakeDynamicClient
	tokens []string
	logs   *bytes.Buffer
}

func newHarness(t *testing.T, objs ...runtime.Object) *harness {
	t.Helper()
	h := &harness{logs: &bytes.Buffer{}}
	h.dc = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{testGVR: "VmSetList"}, objs...)
	s := &Server{
		Clients: func(token string) (dynamic.Interface, error) {
			h.tokens = append(h.tokens, token)
			return h.dc, nil
		},
		Resource: "vmsets",
		Rules: model.Rules{
			Sizes: []string{"M", "S"}, DefaultSize: "S", MaxReplicas: 3,
			SSHKeyFormat: regexp.MustCompile(`^ssh-ed25519 [A-Za-z0-9+/=]+( [A-Za-z0-9@._-]+)?$`),
		},
		Heartbeat: time.Hour,
		Log:       slog.New(slog.NewTextHandler(h.logs, nil)),
	}
	h.srv = httptest.NewServer(s.Handler())
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) do(t *testing.T, method, path, body, token string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp, buf.Bytes()
}

func vmSetObj(name, tenant string, regionPhase string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": name, "namespace": tenant, "labels": map[string]interface{}{model.LabelTenant: tenant}},
		"spec":     map[string]interface{}{"size": "S", "replicas": int64(1)},
	}}
	if regionPhase != "" {
		u.Object["status"] = map[string]interface{}{"regions": []interface{}{
			map[string]interface{}{"region": "eu", "phase": regionPhase, "readyReplicas": int64(1)},
		}}
	}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: model.Group, Version: model.Version, Kind: model.Kind})
	return u
}

func TestRequiresBearerToken(t *testing.T) {
	h := newHarness(t)
	for _, tok := range []string{""} {
		resp, _ := h.do(t, "GET", "/v1/tenants/acme-corp/vmsets/web", "", tok)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
		if resp.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Error("401 should advertise Bearer auth")
		}
	}
	if len(h.tokens) != 0 {
		t.Error("no hub client may be built for an unauthenticated request")
	}
}

func TestCallerTokenIsForwardedAndNeverLogged(t *testing.T) {
	h := newHarness(t, vmSetObj("web", "acme-corp", "Ready"))
	h.do(t, "GET", "/v1/tenants/acme-corp/vmsets/web", "", "s3cret-token")

	if len(h.tokens) != 1 || h.tokens[0] != "s3cret-token" {
		t.Errorf("tokens used = %v, want the caller's", h.tokens)
	}
	if strings.Contains(h.logs.String(), "s3cret-token") {
		t.Errorf("access log leaked the token: %s", h.logs)
	}
	if !strings.Contains(h.logs.String(), "/v1/tenants/acme-corp/vmsets/web") {
		t.Errorf("access log should record the request path: %s", h.logs)
	}
}

func TestGetFlattensStatus(t *testing.T) {
	h := newHarness(t, vmSetObj("web", "acme-corp", "Ready"))
	resp, body := h.do(t, "GET", "/v1/tenants/acme-corp/vmsets/web", "", "tok")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	var got model.VmSet
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "web" || got.Tenant != "acme-corp" || got.Phase != model.PhaseReady || got.Regions[0].Region != "eu" {
		t.Errorf("got %+v", got)
	}
	if strings.Contains(string(body), "managedFields") || strings.Contains(string(body), "apiVersion") {
		t.Errorf("response should not expose Kubernetes plumbing: %s", body)
	}
}

func TestKubeErrorsPassThroughWithTheirStatus(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.do(t, "GET", "/v1/tenants/acme-corp/vmsets/missing", "", "tok")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing object: status = %d, want 404", resp.StatusCode)
	}

	h.dc.PrependReactor("get", "vmsets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(testGVR.GroupResource(), "web", errors.New("no access"))
	})
	resp, body := h.do(t, "GET", "/v1/tenants/other/vmsets/web", "", "tok")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("RBAC denial: status = %d, want 403 (%s)", resp.StatusCode, body)
	}

	h.dc.PrependReactor("get", "vmsets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	resp, _ = h.do(t, "GET", "/v1/tenants/acme-corp/vmsets/web", "", "tok")
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("unreachable hub: status = %d, want 502", resp.StatusCode)
	}
}

func TestCreate(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/v1/tenants/acme-corp/vmsets",
		`{"name":"web","regions":["eu"],"size":"M","replicas":2,"sshPublicKey":"`+goodKey+`"}`, "tok")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); loc != "/v1/tenants/acme-corp/vmsets/web" {
		t.Errorf("Location = %q", loc)
	}
	stored, err := h.dc.Resource(testGVR).Namespace("acme-corp").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("object not stored on the hub: %v", err)
	}
	if stored.GetLabels()[model.LabelTenant] != "acme-corp" {
		t.Errorf("tenant label = %v", stored.GetLabels())
	}
}

func TestCreateCannotClaimAnotherTenant(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.do(t, "POST", "/v1/tenants/acme-corp/vmsets",
		`{"name":"web","regions":["eu"],"tenant":"other","sshPublicKey":"`+goodKey+`"}`, "tok")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown body field: status = %d, want 400", resp.StatusCode)
	}
}

func TestCreateValidationListsProblems(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do(t, "POST", "/v1/tenants/acme-corp/vmsets", `{"name":"Bad","size":"XL"}`, "tok")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Error struct{ Problems []string }
	}
	_ = json.Unmarshal(body, &out)
	if len(out.Error.Problems) < 3 {
		t.Errorf("problems = %v, want several reported together", out.Error.Problems)
	}
	if len(h.dc.Actions()) != 0 {
		t.Errorf("an invalid request must not reach the hub, got %v", h.dc.Actions())
	}
}

func TestCreateConflictPassesThrough(t *testing.T) {
	h := newHarness(t, vmSetObj("web", "acme-corp", ""))
	resp, _ := h.do(t, "POST", "/v1/tenants/acme-corp/vmsets",
		`{"name":"web","regions":["eu"],"sshPublicKey":"`+goodKey+`"}`, "tok")
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestList(t *testing.T) {
	h := newHarness(t, vmSetObj("a", "acme-corp", "Ready"), vmSetObj("b", "acme-corp", "Pending"), vmSetObj("c", "other", "Ready"))
	resp, body := h.do(t, "GET", "/v1/tenants/acme-corp/vmsets", "", "tok")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	var out struct{ Items []model.VmSet }
	_ = json.Unmarshal(body, &out)
	if len(out.Items) != 2 {
		t.Errorf("items = %d, want only the tenant's 2", len(out.Items))
	}

	if resp, _ := h.do(t, "GET", "/v1/tenants/acme-corp/vmsets?limit=9999", "", "tok"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("oversized limit: status = %d, want 400", resp.StatusCode)
	}
}

func TestDelete(t *testing.T) {
	h := newHarness(t, vmSetObj("web", "acme-corp", ""))
	if resp, _ := h.do(t, "DELETE", "/v1/tenants/acme-corp/vmsets/web", "", "tok"); resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
	if resp, _ := h.do(t, "DELETE", "/v1/tenants/acme-corp/vmsets/web", "", "tok"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("second delete: status = %d, want 404", resp.StatusCode)
	}
}

func TestInvalidTenantIsRejected(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.do(t, "GET", "/v1/tenants/Not_Valid/vmsets", "", "tok")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestEventsStreamsSnapshotUpdateAndDelete(t *testing.T) {
	h := newHarness(t, vmSetObj("web", "acme-corp", "Pending"))
	fw := watch.NewFake()
	h.dc.PrependWatchReactor("vmsets", func(k8stesting.Action) (bool, watch.Interface, error) { return true, fw, nil })

	req, _ := http.NewRequest("GET", h.srv.URL+"/v1/tenants/acme-corp/vmsets/web/events", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}

	sc := bufio.NewScanner(resp.Body)
	next := func() (event, data string) {
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				return event, strings.TrimPrefix(line, "data: ")
			}
		}
		t.Fatal("stream ended early")
		return
	}

	if ev, data := next(); ev != "update" || !strings.Contains(data, `"phase":"Pending"`) {
		t.Errorf("snapshot = %s %s", ev, data)
	}
	go fw.Modify(vmSetObj("web", "acme-corp", "Ready"))
	if ev, data := next(); ev != "update" || !strings.Contains(data, `"phase":"Ready"`) {
		t.Errorf("update = %s %s", ev, data)
	}
	go fw.Delete(vmSetObj("web", "acme-corp", "Ready"))
	if ev, _ := next(); ev != "deleted" {
		t.Errorf("event = %s, want deleted", ev)
	}
}
