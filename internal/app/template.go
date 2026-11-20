package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// A pipeline sends app.yaml as it is in the repository, and the values of
// the variables it refers to. Secrets are not written in the file, only
// their names, as ${DATABASE_URL}: the value is in the CI system's secret
// settings, and from there in the pipeline's environment.

// varRe matches a reference to a variable: ${NAME}.
var varRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Variables returns the names of the variables a definition refers to,
// sorted and each once. It looks at the text, comments included: the
// client sends what it has, and the server fills only env values.
func Variables(definition []byte) []string {
	var names []string
	for _, m := range varRe.FindAllSubmatch(definition, -1) {
		if name := string(m[1]); !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// Decode decodes a definition without validating it, for a definition
// that is completed afterwards (its image gets a digest, its env values
// their variables).
func Decode(data []byte) (*App, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var a App
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	return &a, nil
}

// FillEnv replaces each ${NAME} in the env values with vars[NAME].
//
// It works on values, after the YAML has been parsed. Filling the text in
// before parsing, as envsubst does, was tried and is wrong three ways: a
// value with " #" in it is cut there (YAML reads the rest as a comment),
// a value with a line break can add keys of its own, and a variable that
// is not set becomes an empty string without a word. Here a value is put
// in as it is, whatever it contains, and a missing variable is an error.
func (a *App) FillEnv(vars map[string]string) error {
	var missing []string
	for k, v := range a.Env {
		a.Env[k] = varRe.ReplaceAllStringFunc(v, func(ref string) string {
			name := ref[2 : len(ref)-1]
			value, ok := vars[name]
			if !ok && !slices.Contains(missing, name) {
				missing = append(missing, name)
			}
			return value
		})
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("env refers to %s, not set in the pipeline's environment", strings.Join(missing, ", "))
	}
	return nil
}

// Fields returns every value of the definition by its path ("port",
// "env.LOG_LEVEL", "resources.memory"), each encoded the same way every
// time. Two definitions that differ only in how they are written (order,
// comments, quotes) have the same fields; comparing fields says which
// values changed.
func (a *App) Fields() map[string]string {
	f := map[string]string{
		"name":   a.Name,
		"image":  a.Image,
		"domain": a.Domain,
		"port":   strconv.Itoa(a.Port),
	}
	for k, v := range a.Env {
		f["env."+k] = v
	}
	// A list is one value: its order is the user's to choose, but
	// whether anything changed is the same question either way.
	if len(a.Secrets) > 0 {
		f["secrets"] = joinSorted(a.Secrets)
	}
	if len(a.SecretFiles) > 0 {
		f["secret_files"] = joinSorted(a.SecretFiles)
	}
	if a.Resources.Memory != "" {
		f["resources.memory"] = a.Resources.Memory
	}
	if a.Resources.CPUs != 0 {
		f["resources.cpus"] = strconv.FormatFloat(a.Resources.CPUs, 'g', -1, 64)
	}
	if a.Healthcheck != nil {
		f["healthcheck.path"] = a.Healthcheck.Path
	}
	return f
}

// Canonical encodes fields as the bytes a checksum is taken over: JSON,
// whose encoder writes a map's keys in sorted order.
func Canonical(fields map[string]string) []byte {
	b, err := json.Marshal(fields)
	if err != nil {
		// A map of strings always encodes.
		panic(err)
	}
	return b
}

func joinSorted(list []string) string {
	s := slices.Clone(list)
	slices.Sort(s)
	return strings.Join(s, ",")
}
