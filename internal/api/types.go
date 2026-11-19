// Package api is the HTTP interface between the liman CLI and the limand
// daemon: the wire types and the client.
//
// The server (package server) and the client both use these types, so a
// change to one of them breaks both sides at compile time instead of at run
// time. The package deliberately imports nothing of the daemon's internals:
// the CLI links it, and should not carry Docker, Nginx or a database with it.
package api

import "time"

// ErrorResponse is the body of every non-2xx answer.
type ErrorResponse struct {
	Error string `json:"error"`
}

// Kinds of DeployEvent.
const (
	// EventOutput carries a piece of progress output in Text.
	EventOutput = "output"
	// EventConfirm asks the client to approve the step named in Step. The
	// client answers by posting to /v1/confirmations/{ConfirmID}.
	EventConfirm = "confirm"
	// EventResult is always the last event and carries Result.
	EventResult = "result"
)

// DeployEvent is one line of the response to POST /v1/deploys. The response
// is a stream of these, one JSON object per line, sent while the deploy is
// running.
type DeployEvent struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	ConfirmID string `json:"confirm_id,omitempty"`
	Step      string `json:"step,omitempty"`

	Result *DeployResult `json:"result,omitempty"`
}

// DeployResult is the outcome of a deploy.
type DeployResult struct {
	OK bool `json:"ok"`
	// Error explains the failure when OK is false.
	Error string `json:"error,omitempty"`

	App       string `json:"app"`
	Release   string `json:"release,omitempty"`
	Container string `json:"container,omitempty"`
	HostPort  int    `json:"host_port,omitempty"`

	// Domain is the app's domain; Routed says whether the proxy now sends
	// that domain to the new release.
	Domain string `json:"domain,omitempty"`
	Routed bool   `json:"routed"`

	DryRun bool `json:"dry_run"`
}

// ConfirmRequest is the body of POST /v1/confirmations/{id}.
type ConfirmRequest struct {
	Approve bool `json:"approve"`
}

// Container is one entry of GET /v1/apps.
type Container struct {
	App       string `json:"app"`
	Container string `json:"container"`
	Image     string `json:"image"`
	State     string `json:"state"`
	Status    string `json:"status"`
	HostPort  int    `json:"host_port,omitempty"`

	// Role is "current" for the container in service and "previous" for
	// one that is stopped and kept for a rollback.
	Role string `json:"role,omitempty"`

	// Health is the health monitor's verdict: "healthy", "unhealthy",
	// "restarting" or "down". It is empty when the monitor has not looked
	// at the app yet, or is not running.
	Health string `json:"health,omitempty"`
	// HealthDetail explains a verdict other than "healthy".
	HealthDetail string `json:"health_detail,omitempty"`
	// Restarts counts the automatic restarts since the app was last stable.
	Restarts int `json:"restarts,omitempty"`
}

// StopResult is the answer to DELETE /v1/apps/{name}.
type StopResult struct {
	// Removed lists the containers that were stopped and removed.
	Removed []string `json:"removed"`
	// ProxyConfig is the proxy config file that was removed, if any.
	ProxyConfig string `json:"proxy_config,omitempty"`
}

// Health is the answer to GET /v1/health.
type Health struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// Event is one entry of GET /v1/history: an executed step.
type Event struct {
	Time    time.Time `json:"time"`
	App     string    `json:"app"`
	Release string    `json:"release"`
	Step    string    `json:"step"`
	// Action is "run" or "undo".
	Action string `json:"action"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	// DurationMS is how long the step took, in milliseconds.
	DurationMS int64 `json:"duration_ms"`
}

// Secret describes a stored secret. There is deliberately no field for the
// value: the API accepts secret values but never returns them.
type Secret struct {
	Name      string    `json:"name"`
	UpdatedAt time.Time `json:"updated_at"`
}
