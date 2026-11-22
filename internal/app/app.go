// Package app defines the application definition file (app.yaml): its shape,
// how it is loaded and how it is validated.
package app

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"go.yaml.in/yaml/v3"
)

// The kinds of app. A web app serves requests on its domain; a worker serves
// none (a queue consumer, a scheduler, a bot) and has neither domain nor
// port.
const (
	KindWeb    = "web"
	KindWorker = "worker"
)

// The ways a deploy replaces the running release.
const (
	// StrategyOverlap starts the new release next to the old one and stops
	// the old one once the new one is healthy: no gap, but for a moment
	// both run. The default.
	StrategyOverlap = "overlap"
	// StrategyReplace stops the old release first and starts the new one
	// after: never two at once (a bot, a scheduler), but a gap. If the new
	// one is not healthy, the old one is started again.
	StrategyReplace = "replace"
)

// App is the desired state of one application, as written in app.yaml.
//
// The struct tags map YAML keys to fields. Without a tag the decoder would
// look for the lowercased field name, which breaks for names like CPUs.
type App struct {
	// Name identifies the app. It becomes the container name and the Nginx
	// config file name, so it must be a valid DNS label.
	Name string `yaml:"name"`

	// Image is the full image reference including a tag or digest,
	// e.g. ghcr.io/user/orders-api:1.4.
	Image string `yaml:"image"`

	// Kind is "web" (the default) or "worker".
	Kind string `yaml:"kind"`

	// Command replaces the image's command, so that one image can run as
	// the web app and as its worker. Empty keeps the image's.
	Command []string `yaml:"command"`

	// Domain is the public host name that is routed to this app.
	Domain string `yaml:"domain"`

	// Port is the port the app listens on inside the container. The host
	// port is not part of the definition; liman allocates it, unless the
	// app is published on a fixed one.
	Port int `yaml:"port"`

	// Publish puts the app on fixed endpoints of the server, for a load
	// balancer or a proxy liman does not drive. With one endpoint a
	// release replaces the one before; with two they take turns.
	Publish Publish `yaml:"publish"`

	// Env holds environment variables passed to the container.
	Env map[string]string `yaml:"env"`

	// Secrets names values that are not written in this file. The daemon
	// keeps them encrypted and adds them to the container's environment at
	// deploy time, under the same name. Set them with "liman secret set".
	Secrets []string `yaml:"secrets"`

	// SecretFiles names secrets given to the container as files rather
	// than environment variables: each is written to /run/secrets/<NAME>
	// before the container starts, and <NAME>_FILE in the environment
	// says where (the convention of the official Docker images). A file
	// does not show in "docker inspect" and is not inherited by every
	// process the app starts, as an environment variable is.
	SecretFiles []string `yaml:"secret_files"`

	// Resources limits what the container may consume. Zero values mean
	// "no limit".
	Resources Resources `yaml:"resources"`

	// Healthcheck is optional. It is a pointer so that "not set" (nil) can
	// be told apart from "set but empty".
	Healthcheck *Healthcheck `yaml:"healthcheck"`

	// Deploy says how a release replaces the one before.
	Deploy Deploy `yaml:"deploy"`
}

// Worker reports whether the app is a worker: no domain, no port, no proxy.
func (a *App) Worker() bool { return a.Kind == KindWorker }

// Strategy is the deploy strategy, with the default filled in. One fixed
// port cannot hold two releases, so an app published on one replaces.
func (a *App) Strategy() string {
	switch {
	case a.Deploy.Strategy != "":
		return a.Deploy.Strategy
	case len(a.Publish) == 1:
		return StrategyReplace
	default:
		return StrategyOverlap
	}
}

// DefaultSettle is how long a worker without a health check of its own must
// stay up before its deploy counts as a success. A worker that cannot reach
// its queue typically gives up within a few seconds.
const DefaultSettle = 10 * time.Second

// Settle is how long the container must stay up when nothing else tells
// whether it is healthy: healthcheck.settle, or for a worker DefaultSettle.
// Zero means the old rule for a web app without a health check: still
// running after one check interval.
func (a *App) Settle() time.Duration {
	if a.Healthcheck != nil && a.Healthcheck.Settle != 0 {
		return time.Duration(a.Healthcheck.Settle)
	}
	if a.Worker() {
		return DefaultSettle
	}
	return 0
}

// Deploy configures how a release is put into service.
type Deploy struct {
	// Strategy is "overlap" (the default) or "replace".
	Strategy string `yaml:"strategy"`
}

// Duration is a time.Duration written in YAML as "10s" or "1m".
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration; write it like 10s or 1m", node.Line, node.Value)
	}
	*d = Duration(parsed)
	return nil
}

// Resources describes container resource limits.
type Resources struct {
	// Memory is a size such as "512m" or "1g".
	Memory string `yaml:"memory"`

	// CPUs is the number of CPU cores, e.g. 0.5 for half a core.
	CPUs float64 `yaml:"cpus"`
}

// Healthcheck describes how to check that the app can serve requests.
type Healthcheck struct {
	// Path is requested with HTTP GET on the app's port. A worker has no
	// port; an image's own HEALTHCHECK is used when there is one.
	Path string `yaml:"path"`
	// Settle is how long the container must stay up to count as healthy
	// when there is neither a path nor an image HEALTHCHECK.
	Settle Duration `yaml:"settle"`
	// Timeout is how long a release may take to become healthy, however
	// that is decided; zero leaves it to the daemon (30 seconds). A JVM
	// app that takes half a minute to start needs more.
	Timeout Duration `yaml:"timeout"`
}

// Load reads, parses and validates the app definition at path.
func Load(path string) (*App, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		// %w wraps the original error so callers can still inspect it with
		// errors.Is (for example errors.Is(err, fs.ErrNotExist)).
		return nil, fmt.Errorf("read app definition: %w", err)
	}

	a, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return a, nil
}

// Parse decodes and validates an app definition from YAML bytes.
func Parse(data []byte) (*App, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// Reject keys that do not match a struct field. Without this a typo
	// such as "imge:" would be silently ignored.
	dec.KnownFields(true)

	var a App
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}

	if err := a.Validate(); err != nil {
		return nil, fmt.Errorf("invalid app definition:\n%w", err)
	}
	return &a, nil
}

// SecretsDir is where secret files are put inside a container.
const SecretsDir = "/run/secrets"

// SecretFilePath is the path of the secret name inside the container.
func SecretFilePath(name string) string { return SecretsDir + "/" + name }

// SecretFileEnv is the environment variable that holds that path.
func SecretFileEnv(name string) string { return name + "_FILE" }
