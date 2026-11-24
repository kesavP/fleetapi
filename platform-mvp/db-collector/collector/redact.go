package collector

import (
	"net/url"
	"strings"
)

// Redacted is an error whose text has had secrets taken out; it keeps the error it was made from, so that errors.Is still sees what it was
// (a cancelled context, a deadline).
type Redacted struct {
	msg   string
	cause error
}

func (e *Redacted) Error() string { return e.msg }
func (e *Redacted) Unwrap() error { return e.cause }

// Scrub takes a connection string, and the password in it, out of a message: a driver's error can quote the string it was given.
func Scrub(msg, dsn string) string {
	if dsn != "" {
		msg = strings.ReplaceAll(msg, dsn, "<dsn>")
		if u, err := url.Parse(dsn); err == nil && u.User != nil {
			if pw, ok := u.User.Password(); ok && pw != "" {
				msg = strings.ReplaceAll(msg, pw, "***")
			}
		}
	}
	return msg
}

// Redact wraps err with its message scrubbed of the connection string and the given secrets; nil stays nil.
func Redact(err error, dsn string, secrets ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "***")
		}
	}
	return &Redacted{msg: Scrub(msg, dsn), cause: err}
}
