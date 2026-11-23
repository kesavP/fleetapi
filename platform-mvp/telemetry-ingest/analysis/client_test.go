package analysis

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

const testKey = "sk-ant-test-key-DO-NOT-LEAK"

// api is a stand-in for the Messages API: it records the request and answers with what the test sets.
type api struct {
	srv    *httptest.Server
	path   string
	method string
	header http.Header
	body   map[string]any
	status int
	reply  string
}

func newAPI(t *testing.T, reply string) *api {
	t.Helper()
	a := &api{status: http.StatusOK, reply: reply}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.path, a.method, a.header = r.URL.Path, r.Method, r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		a.body = map[string]any{}
		_ = json.Unmarshal(raw, &a.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.reply)
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *api) client() *Client {
	return NewClient(testKey, "claude-opus-5-5", option.WithBaseURL(a.srv.URL), option.WithMaxRetries(0))
}

func message(stop string, blocks ...string) string {
	return `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[` + strings.Join(blocks, ",") + `],"stop_reason":"` + stop + `","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":5}}`
}

func text(s string) string {
	b, _ := json.Marshal(s)
	return `{"type":"text","text":` + string(b) + `}`
}

const thinking = `{"type":"thinking","thinking":"","signature":"sig"}`

func TestTheRequestIsAMessageForTheModelWithTheKeyInTheAuthorizationHeader(t *testing.T) {
	a := newAPI(t, message("end_turn", text("ok")))
	if _, err := a.client().Complete(bg, "the instructions", "the incident"); err != nil {
		t.Fatal(err)
	}
	if a.method != http.MethodPost || a.path != "/v1/messages" {
		t.Fatalf("%s %s", a.method, a.path)
	}
	// the SDK sends the key as a bearer token in the Authorization header, and never anywhere else
	if a.header.Get("Authorization") != "Bearer "+testKey || a.header.Get("Anthropic-Version") == "" {
		t.Fatalf("headers = %v", a.header)
	}
	if a.body["model"] != "claude-opus-5-5" {
		t.Fatalf("model = %v", a.body["model"])
	}
	if mt, _ := a.body["max_tokens"].(float64); mt < 4000 {
		t.Fatalf("max_tokens = %v: thinking and the answer both count against it", a.body["max_tokens"])
	}
	sys, _ := a.body["system"].([]any)
	if len(sys) != 1 || sys[0].(map[string]any)["text"] != "the instructions" {
		t.Fatalf("system = %v", a.body["system"])
	}
	msgs, _ := a.body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", a.body["messages"])
	}
	m := msgs[0].(map[string]any)
	c, _ := m["content"].([]any)
	if m["role"] != "user" || len(c) != 1 || c[0].(map[string]any)["text"] != "the incident" {
		t.Fatalf("message = %v", m)
	}
	for _, not := range []string{"temperature", "top_p", "top_k", "thinking", "tools", "tool_choice"} {
		if _, ok := a.body[not]; ok {
			t.Errorf("the request sets %q, which this call does not (and some models refuse)", not)
		}
	}
}

func TestOnlyTheTextOfTheAnswerIsReturned(t *testing.T) {
	a := newAPI(t, message("end_turn", thinking, text("first part"), text("second part")))
	got, err := a.client().Complete(bg, "s", "u")
	if err != nil {
		t.Fatal(err)
	}
	if got != "first part\nsecond part" {
		t.Fatalf("got %q", got)
	}
}

func TestAModelThatDeclinesAndAnAnswerThatWasCutOffAreErrors(t *testing.T) {
	a := newAPI(t, message("refusal", text("partial")))
	if _, err := a.client().Complete(bg, "s", "u"); !errors.Is(err, ErrDeclined) {
		t.Fatalf("a refusal: err = %v", err)
	}
	a = newAPI(t, message("max_tokens", text("half an answ")))
	if _, err := a.client().Complete(bg, "s", "u"); !errors.Is(err, ErrCutOff) {
		t.Fatalf("an answer that hit max_tokens: err = %v", err)
	}
}

func TestFailuresOfTheCallAreNamedAndNeverCarryTheKeyOrTheReplyBody(t *testing.T) {
	cases := map[int]error{
		http.StatusUnauthorized:        ErrKeyRefused,
		http.StatusForbidden:           ErrKeyRefused,
		http.StatusTooManyRequests:     ErrBusy,
		http.StatusInternalServerError: ErrUnavailable,
		http.StatusBadGateway:          ErrUnavailable,
		http.StatusBadRequest:          ErrRejected,
	}
	for status, want := range cases {
		a := newAPI(t, `{"type":"error","error":{"type":"x","message":"BODY-SENTINEL `+testKey+`"}}`)
		a.status = status
		_, err := a.client().Complete(bg, "s", "u")
		if !errors.Is(err, want) {
			t.Errorf("%d: err = %v, want %v", status, err, want)
			continue
		}
		if strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), "BODY-SENTINEL") {
			t.Errorf("%d: the error repeats the key or the reply: %v", status, err)
		}
	}
}

func TestAServerThatIsNotThereIsUnavailable(t *testing.T) {
	a := newAPI(t, "")
	c := a.client()
	a.srv.Close()
	if _, err := c.Complete(bg, "s", "u"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestACallThatTakesTooLongIsCutOffByItsContext(t *testing.T) {
	a := newAPI(t, message("end_turn", text("ok")))
	ctx, cancel := contextWithCancel()
	cancel()
	if _, err := a.client().Complete(ctx, "s", "u"); err == nil {
		t.Fatal("a call with a cancelled context succeeded")
	}
}
