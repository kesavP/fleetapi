// Package config turns command-line flags into a validated Config. Helm
// values are the source of truth; the flag defaults only keep a bare
// `alert-router` runnable.
package config

import (
	"flag"
	"fmt"
	"io"
	"time"
)

type Config struct {
	Listen      string
	TokenFile   string        // file holding the bearer token Alertmanager must present
	DedupeTTL   time.Duration // 0 disables deduplication
	SinkTimeout time.Duration // per-sink delivery deadline
}

func Parse(args []string) (Config, error) {
	fs := flag.NewFlagSet("alert-router", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var c Config
	fs.StringVar(&c.Listen, "listen", ":8080", "Address to serve on")
	fs.StringVar(&c.TokenFile, "token-file", "", "File holding the bearer token Alertmanager must present (required)")
	fs.DurationVar(&c.DedupeTTL, "dedupe-interval", 10*time.Minute, "Do not forward an unchanged firing alert again within this interval (0 disables)")
	fs.DurationVar(&c.SinkTimeout, "sink-timeout", 5*time.Second, "Deadline for each sink to accept a batch")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	switch {
	case c.TokenFile == "":
		return Config{}, fmt.Errorf("token-file is required")
	case c.DedupeTTL < 0:
		return Config{}, fmt.Errorf("dedupe-interval must not be negative")
	case c.SinkTimeout <= 0:
		return Config{}, fmt.Errorf("sink-timeout must be positive")
	}
	return c, nil
}
