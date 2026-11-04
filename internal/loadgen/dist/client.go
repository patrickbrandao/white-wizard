package dist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client is the worker side of the protocol.
type Client struct {
	// URL of the controller, e.g. http://host:9100.
	URL string
	// Token ≠ "" is sent as "Authorization: Bearer Token".
	Token string
	// HTTP defaults to http.DefaultClient; registration is a long poll, so
	// it should not have a short timeout.
	HTTP *http.Client
}

// Register registers the worker and blocks until the N workers are
// registered (long poll), returning its assignment.
func (c *Client) Register(ctx context.Context, reg Registration) (*Assignment, error) {
	var a Assignment
	if err := c.do(ctx, http.MethodPost, PathRegister, reg, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// SendResult posts the result of the worker.
func (c *Client) SendResult(ctx context.Context, res *Result) error {
	return c.do(ctx, http.MethodPost, PathResult, res, nil)
}

// Status reads the state of the controller.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	var st Status
	if err := c.do(ctx, http.MethodGet, PathStatus, nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	return nil
}
