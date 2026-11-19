// Package app defines the application definition file (app.yaml): its shape,
// how it is loaded and how it is validated.
package app

import (
	"bytes"
	"fmt"
	"os"

	"go.yaml.in/yaml/v3"
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

	// Domain is the public host name that is routed to this app.
	Domain string `yaml:"domain"`

	// Port is the port the app listens on inside the container. The host
	// port is not part of the definition; liman allocates it.
	Port int `yaml:"port"`

	// Env holds environment variables passed to the container.
	Env map[string]string `yaml:"env"`

	// Resources limits what the container may consume. Zero values mean
	// "no limit".
	Resources Resources `yaml:"resources"`

	// Healthcheck is optional. It is a pointer so that "not set" (nil) can
	// be told apart from "set but empty".
	Healthcheck *Healthcheck `yaml:"healthcheck"`
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
	// Path is requested with HTTP GET on the app's port.
	Path string `yaml:"path"`
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
