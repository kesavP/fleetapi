package fleet_test

import (
	"fmt"
	"os"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-client/client/logs"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-client/fleet"
)

// The errors one application logged in the last hour.
func Example() {
	key, err := os.ReadFile(os.Getenv("FLEET_READ_KEY_FILE"))
	if err != nil {
		return
	}
	c, err := fleet.New("https://hub.example.com", "shop", string(key))
	if err != nil {
		return
	}
	from, to := fleet.Since(time.Hour)
	res, err := c.Logs.SearchLogs(logs.NewSearchLogsParams().WithProject(c.Project).
		WithService(fleet.String("/k8s/shop/checkout")).WithSeverity(fleet.String("error")).
		WithFrom(from).WithTo(to).WithLimit(fleet.Int64(20)), nil)
	if err != nil {
		fmt.Println(fleet.Explain(err)) // e.g. "the hub answered 403: this key cannot read that project"
		return
	}
	for _, r := range res.Payload.Data {
		fmt.Println(time.Time(r.Time).Format(time.RFC3339), r.Severity, r.Body)
	}
}
