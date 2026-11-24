// fleetctl reads a project's logs, traces and metrics from the hub, through the generated client (package fleet).
//
// Configuration comes from flags or the environment: --hub / FLEET_HUB, --project / FLEET_PROJECT, and the project's READ key from the file
// named by --key-file / FLEET_READ_KEY_FILE, or from FLEET_READ_KEY. The key is never taken as a flag value (it would land in the shell's
// history) and never printed.
//
// Output is a table for people, or with -o json compact JSON for scripts and for Claude (the fleet-telemetry skill): one array, with the
// fields --fields names (a short default; "all" for everything).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-client/fleet"
)

const usage = `fleetctl reads a project's logs, traces and metrics from the hub.

Usage:
  fleetctl whoami                          check the hub, the project and the read key
  fleetctl logs [flags]                    search logs, newest first
  fleetctl logs services [--since]         the services that sent logs
  fleetctl traces [flags]                  search spans, newest first
  fleetctl traces services [--since]       the services that sent spans
  fleetctl trace <trace-id>                one trace, as a tree of spans
  fleetctl metrics                         the metric names
  fleetctl series <metric> [--match k=v]   a metric's label sets
  fleetctl query <metric> [--match k=v] [--step 60] [--rate]
                                           a metric's values over the time range

Common flags: --hub URL, --project NAME, --key-file FILE, -o table|json, --fields a,b,c|all,
              --since 1h (or --from/--to as RFC 3339 or unix seconds), --limit N.
Environment:  FLEET_HUB, FLEET_PROJECT, FLEET_READ_KEY_FILE (or FLEET_READ_KEY).
Run "fleetctl <command> -h" for a command's flags.
`

// exit codes
const (
	exitOK    = 0
	exitAPI   = 1 // the hub refused or the call failed
	exitUsage = 2 // the command line or the configuration is wrong
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "whoami":
		err = cmdWhoami(rest, getenv, stdout)
	case "logs":
		err = cmdLogs(rest, getenv, stdout)
	case "traces":
		err = cmdTraces(rest, getenv, stdout)
	case "trace":
		err = cmdTrace(rest, getenv, stdout)
	case "metrics":
		err = cmdMetrics(rest, getenv, stdout)
	case "series":
		err = cmdSeries(rest, getenv, stdout)
	case "query":
		err = cmdQuery(rest, getenv, stdout)
	default:
		err = usageError{fmt.Sprintf("unknown command %q (fleetctl help lists them)", cmd)}
	}
	var ue usageError
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, flag.ErrHelp):
		return exitOK
	case errors.As(err, &ue):
		fmt.Fprintln(stderr, "fleetctl:", ue.msg)
		return exitUsage
	default:
		fmt.Fprintln(stderr, "fleetctl:", oneLine(fleet.Explain(err).Error()))
		return exitAPI
	}
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// common is what every command takes.
type common struct {
	fs                *flag.FlagSet
	hub, project, key string // key: the --key-file path
	output, fields    string
	since             time.Duration
	from, to          string
	limit             int64
}

func newCommon(name string, withRange, withLimit bool) *common {
	c := &common{fs: flag.NewFlagSet("fleetctl "+name, flag.ContinueOnError)}
	c.fs.SetOutput(io.Discard)
	c.fs.StringVar(&c.hub, "hub", "", "the hub's URL (FLEET_HUB)")
	c.fs.StringVar(&c.project, "project", "", "the project (FLEET_PROJECT)")
	c.fs.StringVar(&c.key, "key-file", "", "a file holding the project's read key (FLEET_READ_KEY_FILE)")
	c.fs.StringVar(&c.output, "o", "table", "output: table or json")
	c.fs.StringVar(&c.fields, "fields", "", "with -o json: the fields to keep, comma separated, or all")
	if withRange {
		c.fs.DurationVar(&c.since, "since", time.Hour, "how far back, e.g. 15m, 2h, 24h")
		c.fs.StringVar(&c.from, "from", "", "start, RFC 3339 or unix seconds (instead of --since)")
		c.fs.StringVar(&c.to, "to", "", "end, RFC 3339 or unix seconds (default now)")
	}
	if withLimit {
		c.fs.Int64Var(&c.limit, "limit", 100, "how many (at most 1000)")
	}
	return c
}

// parse reads the flags, wherever they are among the positional arguments, and returns the positional ones.
func (c *common) parse(args []string) ([]string, error) {
	var pos []string
	for {
		if err := c.fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				c.fs.SetOutput(os.Stdout)
				c.fs.PrintDefaults()
				return nil, err
			}
			return nil, usageError{err.Error()}
		}
		args = c.fs.Args()
		if len(args) == 0 {
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	if c.output != "table" && c.output != "json" {
		return nil, usageError{"-o must be table or json"}
	}
	if c.since <= 0 && c.fs.Lookup("since") != nil {
		return nil, usageError{"--since must be positive"}
	}
	return pos, nil
}

// timeRange is from and to as the API takes them: --from/--to if given, else the last --since.
func (c *common) timeRange() (from, to *string) {
	to = fleet.Time(time.Now())
	if c.to != "" {
		to = fleet.String(c.to)
	}
	if c.from != "" {
		return fleet.String(c.from), to
	}
	end := time.Now()
	if c.to != "" {
		if t, err := time.Parse(time.RFC3339Nano, c.to); err == nil {
			end = t
		}
	}
	return fleet.Time(end.Add(-c.since)), to
}

// connect builds the client from the flags and the environment.
func (c *common) connect(getenv func(string) string) (*fleet.Client, string, error) {
	hub := firstOf(c.hub, getenv("FLEET_HUB"))
	project := firstOf(c.project, getenv("FLEET_PROJECT"))
	if hub == "" {
		return nil, "", usageError{"no hub: set FLEET_HUB or --hub (e.g. https://hub.example.com)"}
	}
	if project == "" {
		return nil, "", usageError{"no project: set FLEET_PROJECT or --project"}
	}
	key, err := readKey(firstOf(c.key, getenv("FLEET_READ_KEY_FILE")), getenv("FLEET_READ_KEY"))
	if err != nil {
		return nil, "", err
	}
	cl, err := fleet.New(hub, project, key)
	if err != nil {
		return nil, "", usageError{err.Error()}
	}
	return cl, hub, nil
}

func firstOf(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return strings.TrimSpace(x)
		}
	}
	return ""
}

// readKey reads the read key from its file, or takes it from FLEET_READ_KEY. Its value never appears in an error.
func readKey(file, value string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", usageError{fmt.Sprintf("cannot read the key file %s: %v", file, errors.Unwrap(err))}
		}
		if k := strings.TrimSpace(string(b)); k != "" {
			return k, nil
		}
		return "", usageError{"the key file " + file + " is empty"}
	}
	if k := strings.TrimSpace(value); k != "" {
		return k, nil
	}
	return "", usageError{"no read key: set FLEET_READ_KEY_FILE to a file holding the project's read key (or FLEET_READ_KEY)"}
}

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), fleet.DefaultTimeout)
}
