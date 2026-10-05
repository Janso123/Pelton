package jmap

import (
	"context"
	"errors"
	"fmt"

	gojmap "github.com/Janso123/go-jmap"
)

// ErrIndeterminate is returned when a mutation request receives no method
// response. The server may or may not have carried it out, so the client does
// not replay it; a caller that tries again must first check what happened, as
// Submit does by looking for the message in Sent.
var ErrIndeterminate = errors.New("jmap: indeterminate operation: no method response received")

// Client wraps go-jmap with a safe stale-session refresh policy.
//
// Do returns every valid method response. When sessionState changes, the
// Session is refreshed before a later request; this request is never replayed
// solely because SessionStale became true.
type Client struct {
	*gojmap.Client
	refreshBeforeNext bool
}

// NewClient wraps an authenticated (or soon-to-authenticate) go-jmap client.
func NewClient(c *gojmap.Client) *Client {
	return &Client{Client: c}
}

// Do performs a JMAP request. A changed sessionState marks the client to
// refresh before the next call; the response is still returned to the caller.
func (c *Client) Do(ctx context.Context, req *gojmap.Request) (*gojmap.Response, error) {
	if err := c.ensureFresh(ctx); err != nil {
		return nil, err
	}
	resp, err := c.Client.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	if c.Client.SessionStale() {
		c.refreshBeforeNext = true
	}
	return resp, nil
}

// doRead is for idempotent reads. If no method response is received, it
// refreshes the Session once and retries the request.
func (c *Client) doRead(ctx context.Context, req *gojmap.Request) (*gojmap.Response, error) {
	resp, err := c.Do(ctx, req)
	if err == nil {
		return resp, nil
	}
	if rerr := c.Client.RefreshSession(ctx); rerr != nil {
		return nil, err
	}
	c.refreshBeforeNext = false
	resp, err = c.Client.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	if c.Client.SessionStale() {
		c.refreshBeforeNext = true
	}
	return resp, nil
}

// doMutation is for non-idempotent writes. A transport/no-response failure
// returns ErrIndeterminate and is not retried.
func (c *Client) doMutation(ctx context.Context, req *gojmap.Request) (*gojmap.Response, error) {
	resp, err := c.Do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIndeterminate, err)
	}
	return resp, nil
}

func (c *Client) ensureFresh(ctx context.Context) error {
	if !c.refreshBeforeNext {
		return nil
	}
	if err := c.Client.RefreshSession(ctx); err != nil {
		return err
	}
	c.refreshBeforeNext = false
	return nil
}
