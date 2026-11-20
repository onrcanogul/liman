package app

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Compiled once at package initialisation. MustCompile panics on an invalid
// pattern, which is fine for patterns that are fixed in the source code.
var (
	// A DNS label: lowercase letters, digits and hyphens, not starting or
	// ending with a hyphen, at most 63 characters.
	nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

	// Two or more DNS labels separated by dots.
	domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

	envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

	// A positive integer followed by a unit, the same notation Docker uses.
	memoryRe = regexp.MustCompile(`^([1-9][0-9]*)([kmg])$`)

	// The characters an image reference can contain. This is an allowlist:
	// anything not listed is rejected, including spaces, quotes, shell
	// metacharacters and terminal control sequences.
	imageRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)

	// A URL path, optionally with a query string, from the same kind of
	// allowlist.
	healthPathRe = regexp.MustCompile(`^/[A-Za-z0-9._~%/?&=+:@-]*$`)
)

// Limits on the size of individual values. The whole file is already
// bounded by the API; these keep single fields within what the systems
// they are handed to accept.
const (
	maxImageLength      = 255
	maxDomainLength     = 253
	maxHealthPathLength = 200
	maxEnvVars          = 200
)

// ValidName reports whether name is a valid app name. The name ends up in
// container names, file names and URLs, so every place that receives one
// from outside must check it with this function.
func ValidName(name string) bool {
	return nameRe.MatchString(name)
}

// The three functions below check values that liman wrote itself and reads
// back later from somewhere it does not control (container labels). They
// apply the same rules as Validate does to a definition.

// ValidDomain reports whether domain is a valid domain for an app.
func ValidDomain(domain string) bool {
	return len(domain) <= maxDomainLength && domainRe.MatchString(domain)
}

// ValidHealthPath reports whether path is a valid health check path. It
// must start with a slash: the path is appended to "http://127.0.0.1:port",
// and anything else (an "@host", say) would change where the request goes.
func ValidHealthPath(path string) bool {
	return len(path) <= maxHealthPathLength && healthPathRe.MatchString(path)
}

// releaseRe is the shape of a release name: a timestamp, or whatever a test
// uses in its place.
var releaseRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidRelease reports whether release is a valid release name.
func ValidRelease(release string) bool {
	return releaseRe.MatchString(release)
}

// Validate checks that the definition describes a deployable app.
//
// It collects every problem instead of stopping at the first one, so the
// user can fix the whole file in a single pass.
func (a *App) Validate() error {
	var errs []error

	if !nameRe.MatchString(a.Name) {
		errs = append(errs, fmt.Errorf("name: must be 1-63 lowercase letters, digits or hyphens, got %q", a.Name))
	}

	if err := validateImage(a.Image); err != nil {
		errs = append(errs, fmt.Errorf("image: %w", err))
	}

	if len(a.Domain) > maxDomainLength || !domainRe.MatchString(a.Domain) {
		errs = append(errs, fmt.Errorf("domain: must be a lowercase host name such as api.example.com, got %q", a.Domain))
	}

	if a.Port < 1 || a.Port > 65535 {
		errs = append(errs, fmt.Errorf("port: must be between 1 and 65535, got %d", a.Port))
	}

	if len(a.Env) > maxEnvVars {
		errs = append(errs, fmt.Errorf("env: at most %d variables are allowed, got %d", maxEnvVars, len(a.Env)))
	}

	// Map iteration order is random in Go. Sort the keys so the error
	// output is the same on every run.
	keys := make([]string, 0, len(a.Env))
	for k := range a.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !envKeyRe.MatchString(k) {
			errs = append(errs, fmt.Errorf("env: invalid variable name %q", k))
		}
		// A NUL byte ends a string in C, which is what the kernel uses for
		// the environment: everything after it would silently disappear.
		if strings.ContainsRune(a.Env[k], 0) {
			errs = append(errs, fmt.Errorf("env: value of %s contains a NUL byte", k))
		}
	}

	if len(a.Secrets) > maxEnvVars {
		errs = append(errs, fmt.Errorf("secrets: at most %d secrets are allowed, got %d", maxEnvVars, len(a.Secrets)))
	}
	seen := map[string]bool{}
	for _, name := range a.Secrets {
		switch {
		case !envKeyRe.MatchString(name):
			errs = append(errs, fmt.Errorf("secrets: invalid name %q", name))
		case seen[name]:
			errs = append(errs, fmt.Errorf("secrets: %s is listed twice", name))
		}
		seen[name] = true
		// A name in both places would leave it unclear which value wins.
		if _, clash := a.Env[name]; clash {
			errs = append(errs, fmt.Errorf("secrets: %s is also set under env; keep it in one place", name))
		}
	}

	if len(a.SecretFiles) > maxEnvVars {
		errs = append(errs, fmt.Errorf("secret_files: at most %d are allowed, got %d", maxEnvVars, len(a.SecretFiles)))
	}
	seenFile := map[string]bool{}
	for _, name := range a.SecretFiles {
		switch {
		case !envKeyRe.MatchString(name):
			errs = append(errs, fmt.Errorf("secret_files: invalid name %q", name))
			continue
		case seenFile[name]:
			errs = append(errs, fmt.Errorf("secret_files: %s is listed twice", name))
		case seen[name]:
			// In both lists the value would be in the environment anyway,
			// which is what the file is there to avoid.
			errs = append(errs, fmt.Errorf("secret_files: %s is also listed under secrets; a secret is given either as a file or in the environment", name))
		}
		seenFile[name] = true
		pointer := SecretFileEnv(name)
		if _, clash := a.Env[pointer]; clash {
			errs = append(errs, fmt.Errorf("secret_files: %s is set under env, but liman sets it to the file's path", pointer))
		}
		if seen[pointer] {
			errs = append(errs, fmt.Errorf("secret_files: %s is listed under secrets, but liman sets it to the file's path", pointer))
		}
	}

	if a.Resources.Memory != "" {
		if _, err := a.Resources.MemoryBytes(); err != nil {
			errs = append(errs, fmt.Errorf("resources.memory: %w", err))
		}
	}

	if a.Resources.CPUs < 0 {
		errs = append(errs, fmt.Errorf("resources.cpus: must not be negative, got %g", a.Resources.CPUs))
	}

	if a.Healthcheck != nil {
		path := a.Healthcheck.Path
		switch {
		case !strings.HasPrefix(path, "/"):
			errs = append(errs, fmt.Errorf("healthcheck.path: must start with \"/\", got %q", path))
		case len(path) > maxHealthPathLength:
			errs = append(errs, fmt.Errorf("healthcheck.path: must be at most %d characters", maxHealthPathLength))
		case !healthPathRe.MatchString(path):
			errs = append(errs, fmt.Errorf("healthcheck.path: contains characters that are not allowed in a URL path, got %q", path))
		}
	}

	// errors.Join returns nil when errs is empty, so no special case is
	// needed for the valid path.
	return errors.Join(errs...)
}

// validateImage requires an explicit, fixed version. Without one Docker
// falls back to "latest", and then nobody can tell which version is running
// or what a rollback should go back to.
func validateImage(image string) error {
	if image == "" {
		return errors.New("is required")
	}
	if len(image) > maxImageLength {
		return fmt.Errorf("must be at most %d characters", maxImageLength)
	}
	// %q prints the value quoted with control characters escaped, so a
	// hostile value cannot smuggle escape sequences into the terminal
	// through this error message.
	if !imageRe.MatchString(image) {
		return fmt.Errorf("contains characters that are not allowed in an image reference, got %q", image)
	}

	// A digest reference (name@sha256:...) is always a fixed version.
	if strings.Contains(image, "@") {
		return nil
	}

	// The tag is whatever follows a colon in the last path segment. Looking
	// only at the last segment avoids mistaking a registry port
	// (localhost:5000/app) for a tag.
	last := image[strings.LastIndex(image, "/")+1:]
	_, tag, found := strings.Cut(last, ":")
	if !found || tag == "" {
		return fmt.Errorf("must include a tag such as :1.4, got %q", image)
	}
	if tag == "latest" {
		return errors.New(`must use a fixed tag, not "latest"`)
	}
	return nil
}

// MemoryBytes converts Memory ("512m") to a number of bytes. It returns 0
// when no limit is set.
func (r Resources) MemoryBytes() (int64, error) {
	if r.Memory == "" {
		return 0, nil
	}

	m := memoryRe.FindStringSubmatch(r.Memory)
	if m == nil {
		return 0, fmt.Errorf("must be a number followed by k, m or g (for example 512m), got %q", r.Memory)
	}

	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number in %q: %w", r.Memory, err)
	}

	switch m[2] {
	case "k":
		n *= 1 << 10
	case "m":
		n *= 1 << 20
	case "g":
		n *= 1 << 30
	}
	return n, nil
}
