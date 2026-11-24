package analysis

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// What can go wrong with the call, in words that are safe to pass on: none of them carries the key or what the API said.
var (
	ErrKeyRefused  = errors.New("the Anthropic API refused the key it was given")
	ErrBusy        = errors.New("the Anthropic API is rate limiting this key; try again in a minute")
	ErrUnavailable = errors.New("the Anthropic API could not be reached or had a failure")
	ErrRejected    = errors.New("the Anthropic API did not accept the request")
	ErrDeclined    = errors.New("the model declined to answer")
	ErrCutOff      = errors.New("the answer was cut off before it was finished")
)

// maxTokens is what the answer may take, and thinking takes from it too: the answer itself is short (see the instructions).
const maxTokens = 16000

// Client is a Completer that asks the Anthropic Messages API.
type Client struct {
	c     anthropic.Client
	model string
}

// NewClient asks `model` with `key`. Further options are for a test (a different address, no retries).
func NewClient(key, model string, opts ...option.RequestOption) *Client {
	return &Client{c: anthropic.NewClient(append([]option.RequestOption{option.WithAPIKey(key)}, opts...)...), model: model}
}

// Complete sends one message and returns the text of the answer. It sets no thinking, sampling or tool option: thinking is on by default on the
// models this is used with, and some of them refuse the others.
func (c *Client) Complete(ctx context.Context, system, user string) (string, error) {
	resp, err := c.c.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: maxTokens,
		System:    []anthropic.TextBlockParam{{Text: system}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))},
	})
	if err != nil {
		return "", classify(err)
	}
	switch resp.StopReason {
	case anthropic.StopReasonRefusal:
		return "", ErrDeclined
	case anthropic.StopReasonMaxTokens:
		return "", ErrCutOff
	}
	var parts []string
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok && strings.TrimSpace(t.Text) != "" {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n"), nil
}

// classify names a failure of the call without repeating what the API or the network said.
func classify(err error) error {
	var ae *anthropic.Error
	if errors.As(err, &ae) {
		switch {
		case ae.StatusCode == http.StatusUnauthorized || ae.StatusCode == http.StatusForbidden:
			return ErrKeyRefused
		case ae.StatusCode == http.StatusTooManyRequests:
			return ErrBusy
		case ae.StatusCode >= 500:
			return fmt.Errorf("%w (status %d)", ErrUnavailable, ae.StatusCode)
		default:
			return fmt.Errorf("%w (status %d)", ErrRejected, ae.StatusCode)
		}
	}
	return ErrUnavailable
}
