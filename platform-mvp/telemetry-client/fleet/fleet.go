// Package fleet connects the generated telemetry client (packages client/logs, client/traces and client/metrics, generated from
// api/swagger.json) to a hub, so a caller does not repeat the transport and the key:
//
//	c, err := fleet.New("https://hub.example.com", "shop", readKey)
//	from, to := fleet.Since(time.Hour)
//	res, err := c.Logs.SearchLogs(logs.NewSearchLogsParams().WithProject(c.Project).
//		WithService(fleet.String("/k8s/shop/checkout")).WithSeverity(fleet.String("error")).WithFrom(from).WithTo(to), nil)
//
// The key is a project's READ key: it reads that project and nothing else, and cannot change anything. It is sent in the X-API-Key header
// on every call and is not kept anywhere else.
package fleet

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-openapi/runtime"
	httptransport "github.com/go-openapi/runtime/client"
	"github.com/go-openapi/strfmt"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-client/client"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-client/client/logs"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-client/client/metrics"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-client/client/traces"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-client/models"
)

// DefaultTimeout is how long one call may take unless WithHTTPClient gives another client.
const DefaultTimeout = 60 * time.Second

// Client is the generated services, bound to one hub, project and key.
type Client struct {
	Project string
	Logs    logs.ClientService
	Traces  traces.ClientService
	Metrics metrics.ClientService
}

type options struct {
	httpClient *http.Client
}

// Option changes how New connects.
type Option func(*options)

// WithHTTPClient uses this HTTP client (its timeout, TLS settings, proxy).
func WithHTTPClient(c *http.Client) Option { return func(o *options) { o.httpClient = c } }

// New returns a client of the hub at hubURL (scheme, host and an optional path prefix, e.g. https://hub.example.com or
// http://localhost:8080/telemetry) for the project, authenticated with its read key.
func New(hubURL, project, readKey string, opts ...Option) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(hubURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("the hub must be an http(s) URL such as https://hub.example.com, not %q", hubURL)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("the hub URL must not have a query or a fragment")
	}
	if strings.TrimSpace(project) == "" {
		return nil, errors.New("a project is needed")
	}
	if strings.TrimSpace(readKey) == "" {
		return nil, errors.New("a read key is needed")
	}
	o := options{httpClient: &http.Client{Timeout: DefaultTimeout}}
	for _, opt := range opts {
		opt(&o)
	}
	basePath := strings.TrimRight(u.Path, "/") + client.DefaultBasePath
	t := httptransport.NewWithClient(u.Host, basePath, []string{u.Scheme}, o.httpClient)
	t.DefaultAuthentication = httptransport.APIKeyAuth("X-API-Key", "header", strings.TrimSpace(readKey))
	c := client.New(t, strfmt.Default)
	return &Client{Project: project, Logs: c.Logs, Traces: c.Traces, Metrics: c.Metrics}, nil
}

// String, Int64 and Bool return a pointer, for the generated parameters' optional fields.
func String(s string) *string { return &s }
func Int64(n int64) *int64    { return &n }
func Bool(b bool) *bool       { return &b }

// Time formats a time as the API takes it (RFC 3339, UTC).
func Time(t time.Time) *string { return String(t.UTC().Format(time.RFC3339Nano)) }

// Since is the time range of the last d: from and to, ready for WithFrom and WithTo.
func Since(d time.Duration) (from, to *string) {
	now := time.Now()
	return Time(now.Add(-d)), Time(now)
}

// Match is a label matcher for the metrics calls (WithMatch): label=value.
func Match(label, value string) string { return label + "=" + value }

// payloadError is what every generated error answer has: the {"error": "..."} the hub sent.
type payloadError interface {
	GetPayload() *models.Error
	Code() int
}

// APIError is an answer of the hub that is not a success: its HTTP status and its message.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("the hub answered %d: %s", e.Status, e.Message) }

// Explain turns what a call returned as an error into an *APIError when the hub answered (so a caller can read the status and the hub's
// message), and returns any other error (the network, a timeout, a parameter the client refused) as it is.
func Explain(err error) error {
	if err == nil {
		return nil
	}
	var pe payloadError
	if errors.As(err, &pe) {
		msg := http.StatusText(pe.Code())
		if p := pe.GetPayload(); p != nil && p.Error != nil && *p.Error != "" {
			msg = *p.Error
		}
		return &APIError{Status: pe.Code(), Message: msg}
	}
	var ae *runtime.APIError
	if errors.As(err, &ae) {
		return &APIError{Status: ae.Code, Message: http.StatusText(ae.Code)}
	}
	return err
}
