package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Client talks to a limand daemon.
type Client struct {
	// BaseURL is the daemon's address, e.g. http://127.0.0.1:7070.
	BaseURL string
	// Token authenticates the client.
	Token string
	// HTTP is the client used for requests. Nil means a default client
	// without a timeout: deploys and followed logs are long-lived streams,
	// and a client timeout would cut them off. Cancellation comes from the
	// context instead.
	HTTP *http.Client
}

// Error is a non-2xx answer from the daemon.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	return e.Message
}

// ErrDeployFailed is wrapped by the error Deploy and Rollback return when
// the daemon ran the operation and it did not succeed.
var ErrDeployFailed = errors.New("deploy failed")

// DeployOptions selects how the daemon executes a deploy or a rollback.
type DeployOptions struct {
	DryRun bool
	// Confirm, when set, enables step-by-step mode. It is called with the
	// name of each step and returns whether to run it.
	Confirm func(step string) (bool, error)
}

// Deploy sends an app definition to the daemon and copies the progress
// output to out while the deploy runs.
func (c *Client) Deploy(ctx context.Context, definition []byte, opts DeployOptions, out io.Writer) (*DeployResult, error) {
	return c.runStreamed(ctx, "/v1/deploys", "application/yaml", bytes.NewReader(definition), opts, out)
}

// Rollback asks the daemon to put the previous release of an app back into
// service, and copies the progress output to out.
func (c *Client) Rollback(ctx context.Context, app string, opts DeployOptions, out io.Writer) (*DeployResult, error) {
	return c.runStreamed(ctx, "/v1/apps/"+url.PathEscape(app)+"/rollback", "", nil, opts, out)
}

// runStreamed posts to an endpoint that answers with a stream of
// DeployEvents and follows that stream to its result.
func (c *Client) runStreamed(ctx context.Context, path, contentType string, body io.Reader, opts DeployOptions, out io.Writer) (*DeployResult, error) {
	query := url.Values{}
	if opts.DryRun {
		query.Set("dry_run", "true")
	}
	if opts.Confirm != nil {
		query.Set("step", "true")
	}

	resp, err := c.do(ctx, http.MethodPost, path, query, contentType, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	// A Decoder reads one JSON value per call from a stream, however the
	// bytes happen to be split into network packets. Each Decode blocks
	// until the daemon has sent the next event.
	dec := json.NewDecoder(resp.Body)
	for {
		var ev DeployEvent
		if err := dec.Decode(&ev); err != nil {
			// The stream ended without a result event: the daemon died or
			// the connection broke. The outcome is unknown.
			if errors.Is(err, io.EOF) {
				return nil, errors.New("connection to the daemon was lost before the operation finished; check with: liman ps")
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("read progress: %w", err)
		}

		switch ev.Type {
		case EventOutput:
			if _, err := io.WriteString(out, ev.Text); err != nil {
				return nil, err
			}

		case EventConfirm:
			approve := false
			if opts.Confirm != nil {
				// A failure to get an answer (for example the terminal was
				// closed) counts as "no": the safe choice is not to proceed.
				approve, _ = opts.Confirm(ev.Step)
			}
			if err := c.confirm(ctx, ev.ConfirmID, approve); err != nil {
				return nil, err
			}

		case EventResult:
			if ev.Result == nil {
				return nil, errors.New("daemon sent a result event without a result")
			}
			if !ev.Result.OK {
				return ev.Result, fmt.Errorf("%w: %s", ErrDeployFailed, ev.Result.Error)
			}
			return ev.Result, nil

			// Unknown event types are skipped, so an older client keeps
			// working when a newer daemon adds one.
		}
	}
}

func (c *Client) confirm(ctx context.Context, id string, approve bool) error {
	body, err := json.Marshal(ConfirmRequest{Approve: approve})
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/confirmations/"+url.PathEscape(id), nil, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("send answer: %w", err)
	}
	return resp.Body.Close()
}

// Apps lists the containers the daemon manages.
func (c *Client) Apps(ctx context.Context) ([]Container, error) {
	var out []Container
	err := c.getJSON(ctx, "/v1/apps", nil, &out)
	return out, err
}

// Logs copies the logs of an app to out. With follow it keeps going until
// ctx is cancelled or the container stops.
func (c *Client) Logs(ctx context.Context, app string, follow bool, out io.Writer) error {
	query := url.Values{}
	if follow {
		query.Set("follow", "true")
	}

	resp, err := c.do(ctx, http.MethodGet, "/v1/apps/"+url.PathEscape(app)+"/logs", query, "", nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	// io.Copy reads until the body ends, writing each chunk as it arrives.
	if _, err := io.Copy(out, resp.Body); err != nil && ctx.Err() == nil {
		return fmt.Errorf("read logs: %w", err)
	}
	return nil
}

// Stop stops and removes an app.
func (c *Client) Stop(ctx context.Context, app string) (*StopResult, error) {
	resp, err := c.do(ctx, http.MethodDelete, "/v1/apps/"+url.PathEscape(app), nil, "", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var out StopResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return &out, nil
}

// History returns the newest limit executed steps, oldest first. An empty
// app returns the steps of all apps.
func (c *Client) History(ctx context.Context, app string, limit int) ([]Event, error) {
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if app != "" {
		query.Set("app", app)
	}

	var out []Event
	err := c.getJSON(ctx, "/v1/history", query, &out)
	return out, err
}

// SetSecret stores value as the secret name of app.
func (c *Client) SetSecret(ctx context.Context, app, name string, value []byte) error {
	// The value travels in the request body, never in the URL: URLs end up
	// in logs and histories.
	resp, err := c.do(ctx, http.MethodPut, secretPath(app, name), nil, "application/octet-stream", bytes.NewReader(value))
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Secrets lists the names of the secrets of app.
func (c *Client) Secrets(ctx context.Context, app string) ([]Secret, error) {
	var out []Secret
	err := c.getJSON(ctx, "/v1/apps/"+url.PathEscape(app)+"/secrets", nil, &out)
	return out, err
}

// DeleteSecret removes the secret name of app.
func (c *Client) DeleteSecret(ctx context.Context, app, name string) error {
	resp, err := c.do(ctx, http.MethodDelete, secretPath(app, name), nil, "", nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func secretPath(app, name string) string {
	return "/v1/apps/" + url.PathEscape(app) + "/secrets/" + url.PathEscape(name)
}

// Health asks the daemon whether it is up. It needs no token.
func (c *Client) Health(ctx context.Context) (*Health, error) {
	var out Health
	if err := c.getJSON(ctx, "/v1/health", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, into any) error {
	resp, err := c.do(ctx, http.MethodGet, path, query, "", nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	return nil
}

// do sends a request and returns the response if its status is 2xx. Any
// other status is turned into an *Error and the body is closed.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, contentType string, body io.Reader) (*http.Response, error) {
	target := strings.TrimSuffix(c.BaseURL, "/") + path
	if len(query) > 0 {
		// Encode escapes the values, so nothing in them can alter the URL.
		target += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("cannot reach the liman daemon at %s (is limand running?): %w", c.BaseURL, err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()

	apiErr := &Error{Status: resp.StatusCode}
	var parsed ErrorResponse
	// The body should be our JSON error, but a proxy in between might
	// answer with something else; fall back to the status text.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&parsed); err == nil && parsed.Error != "" {
		apiErr.Message = parsed.Error
	} else {
		apiErr.Message = "daemon answered " + resp.Status
	}
	if resp.StatusCode == http.StatusUnauthorized {
		apiErr.Message += "; set the token in client.yaml or LIMAN_TOKEN (create one on the server with: limand token create <name>)"
	}
	return nil, apiErr
}
