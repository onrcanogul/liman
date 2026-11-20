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

	// Release, Domain and Healthcheck are what the container was deployed
	// with; Created is when it was created. A client that only has the
	// listing (the panel) could not know them otherwise.
	Release     string    `json:"release,omitempty"`
	Domain      string    `json:"domain,omitempty"`
	Healthcheck string    `json:"healthcheck,omitempty"`
	Created     time.Time `json:"created"`

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
	// KeptSecrets names the app's secrets, which a stop leaves in place:
	// deploying the app again needs them.
	KeptSecrets []string `json:"kept_secrets,omitempty"`
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

// Release is one entry of GET /v1/releases: a deploy and how it ended.
type Release struct {
	App     string `json:"app"`
	Release string `json:"release"`
	Image   string `json:"image"`
	// Status is "succeeded" or "failed".
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// HealthHistory is the answer to GET /v1/apps/{name}/health: the health
// checks of an app over a stretch of time, summarised in buckets of equal
// width.
type HealthHistory struct {
	App   string    `json:"app"`
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
	// BucketSeconds is the width of one bucket.
	BucketSeconds int `json:"bucket_seconds"`
	// Buckets is ordered oldest first. A stretch in which the app was not
	// checked at all has no entry: no data is not the same as healthy.
	Buckets []HealthBucket `json:"buckets"`
}

// HealthBucket summarises the health checks within one bucket.
type HealthBucket struct {
	Start  time.Time `json:"start"`
	OK     int       `json:"ok"`
	Failed int       `json:"failed"`
	// Probed counts the checks that measured a response time. AvgMS and
	// MaxMS describe those; they are left out when there were none.
	Probed int     `json:"probed"`
	AvgMS  float64 `json:"avg_ms,omitempty"`
	MaxMS  int64   `json:"max_ms,omitempty"`
}

// Info is the answer to GET /v1/info: what this daemon is and how it is
// set up. Unlike GET /v1/health it needs a token.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Built   string `json:"built"`

	// StartedAt is when the daemon started; Now is its clock at the time of
	// the answer, so a client can tell if its own clock disagrees.
	StartedAt time.Time `json:"started_at"`
	Now       time.Time `json:"now"`

	// Token and Scope describe the token the question was asked with: its
	// name and whether it is "admin" or "read". A client uses them to
	// offer only what will be allowed.
	Token string `json:"token"`
	Scope string `json:"scope"`
	// TokenExpiresAt is when that token stops working; absent when it
	// never does. A pointer, because omitempty leaves out nil but not a
	// zero time.Time: a struct is never "empty" to encoding/json.
	TokenExpiresAt *time.Time `json:"token_expires_at,omitempty"`

	// Proxy says whether the daemon manages a reverse proxy, Secrets whether
	// it has secret storage, Backups whether it can make backups.
	Proxy   bool `json:"proxy"`
	Secrets bool `json:"secrets"`
	Backups bool `json:"backups"`

	// Monitor describes the health monitor; it is absent when the monitor
	// is turned off.
	Monitor *MonitorInfo `json:"monitor,omitempty"`
}

// MonitorInfo describes the health monitor's settings.
type MonitorInfo struct {
	IntervalSeconds  int `json:"interval_seconds"`
	FailureThreshold int `json:"failure_threshold"`
	// RetentionSeconds is how long health checks are kept.
	RetentionSeconds int `json:"retention_seconds"`
}

// Backup describes one backup file of the daemon's database.
type Backup struct {
	Name      string    `json:"name"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// BackupList is the answer to GET /v1/backups.
type BackupList struct {
	// Dir is the directory on the server the backups are in.
	Dir string `json:"dir"`
	// Keep is how many backups stay after a new one is made; zero means
	// all of them.
	Keep int `json:"keep"`
	// Backups is ordered newest first.
	Backups []Backup `json:"backups"`
}

// BackupReport is what checking a backup found. A report exists only for a
// file that could be opened and is not damaged; OK additionally says that
// every secret in it can be decrypted with the daemon's master key.
type BackupReport struct {
	OK            bool           `json:"ok"`
	SchemaVersion int            `json:"schema_version"`
	Rows          map[string]int `json:"rows"`
	Secrets       int            `json:"secrets"`
	// KeyChecked is false when the secrets could not be tested because the
	// daemon has no master key.
	KeyChecked bool `json:"key_checked"`
	// Signature says whether the file is what the daemon's master key
	// signed: "valid", "missing" (made before backups were signed),
	// "invalid" (changed since, or from another key) or "unchecked" (the
	// daemon has no master key).
	Signature string `json:"signature"`
	// Undecryptable names the secrets the master key does not open, as
	// app/name. Names only: no value ever leaves the daemon.
	Undecryptable []string `json:"undecryptable,omitempty"`
}

// BackupResult is the answer to POST /v1/backups.
type BackupResult struct {
	Backup Backup       `json:"backup"`
	Report BackupReport `json:"report"`
	// Removed is how many older backups were removed afterwards.
	Removed int `json:"removed"`
}
