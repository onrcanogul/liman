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

// Releases returns the newest limit deploys, newest first. An empty app
// returns the deploys of all apps.
func (c *Client) Releases(ctx context.Context, app string, limit int) ([]Release, error) {
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if app != "" {
		query.Set("app", app)
	}

	var out []Release
	err := c.getJSON(ctx, "/v1/releases", query, &out)
	return out, err
}

// HealthHistory returns the health checks of app over the given range,
// which is one of "6h", "24h" and "7d".
func (c *Client) HealthHistory(ctx context.Context, app, span string) (*HealthHistory, error) {
	var out HealthHistory
	if err := c.getJSON(ctx, "/v1/apps/"+url.PathEscape(app)+"/health", url.Values{"range": {span}}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Info describes the daemon.
func (c *Client) Info(ctx context.Context) (*Info, error) {
	var out Info
	if err := c.getJSON(ctx, "/v1/info", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Certificates lists the certificates in the server's nginx.cert_dir.
func (c *Client) Certificates(ctx context.Context) (*Certificates, error) {
	var out Certificates
	if err := c.getJSON(ctx, "/v1/certs", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Keys lists the server's master keys and what depends on each.
func (c *Client) Keys(ctx context.Context) (*Keys, error) {
	var out Keys
	if err := c.getJSON(ctx, "/v1/keys", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Backups lists the backups on the server, newest first.
func (c *Client) Backups(ctx context.Context) (*BackupList, error) {
	var out BackupList
	if err := c.getJSON(ctx, "/v1/backups", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateBackup makes a backup now.
func (c *Client) CreateBackup(ctx context.Context) (*BackupResult, error) {
	var out BackupResult
	if err := c.postJSON(ctx, "/v1/backups", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// VerifyBackup checks the backup called name.
func (c *Client) VerifyBackup(ctx context.Context, name string) (*BackupReport, error) {
	var out BackupReport
	if err := c.postJSON(ctx, "/v1/backups/"+url.PathEscape(name)+"/verify", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// postJSON sends a POST without a body and decodes the JSON answer.
func (c *Client) postJSON(ctx context.Context, path string, into any) error {
	resp, err := c.do(ctx, http.MethodPost, path, nil, "", nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	return nil
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

// AllSecrets lists the secrets of every app by name, apps that are not
// deployed included.
func (c *Client) AllSecrets(ctx context.Context) ([]Secret, error) {
	var out []Secret
	err := c.getJSON(ctx, "/v1/secrets", nil, &out)
	return out, err
}

// Tokens lists the API tokens, without the tokens themselves.
func (c *Client) Tokens(ctx context.Context) ([]Token, error) {
	var out []Token
	err := c.getJSON(ctx, "/v1/tokens", nil, &out)
	return out, err
}

// RevokeToken revokes the token called name. Revoking the token the
// client itself uses is allowed; the next request is then refused.
func (c *Client) RevokeToken(ctx context.Context, name string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/v1/tokens/"+url.PathEscape(name), nil, "", nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Audit returns the newest limit entries of the audit trail, of app or of
// everything this token may see when app is "".
func (c *Client) Audit(ctx context.Context, app string, limit int) ([]AuditEntry, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if app != "" {
		q.Set("app", app)
	}
	var out []AuditEntry
	err := c.getJSON(ctx, "/v1/audit", q, &out)
	return out, err
}

// VerifyAudit asks the daemon to check the whole audit trail.
func (c *Client) VerifyAudit(ctx context.Context) (*AuditReport, error) {
	var out AuditReport
	if err := c.getJSON(ctx, "/v1/audit/verify", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
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
