package app

import (
	"bytes"
	"maps"
	"slices"
	"strings"
	"testing"
)

const template = `
name: orders-api
image: ghcr.io/team/orders-api
domain: api.example.com
port: 3000
env:
  LOG_LEVEL: info
  DATABASE_URL: ${DATABASE_URL}
  STRIPE_KEY: ${STRIPE_KEY}
  GREETING: hello ${NAME}, ${NAME}
`

func TestVariables(t *testing.T) {
	got := Variables([]byte(template + "# ${IN_A_COMMENT}\n"))
	want := []string{"DATABASE_URL", "IN_A_COMMENT", "NAME", "STRIPE_KEY"}
	if !slices.Equal(got, want) {
		t.Errorf("Variables() = %v, want %v", got, want)
	}
}

func TestFillEnvPutsValuesInAsTheyAre(t *testing.T) {
	a, err := Decode([]byte(template))
	if err != nil {
		t.Fatal(err)
	}
	// What broke filling in the text: a comment sign, a line break that
	// would add keys, YAML syntax.
	vars := map[string]string{
		"DATABASE_URL": "postgres://u:p #ss@db/x",
		"STRIPE_KEY":   "sk\ndomain: evil.example\nimage: evil/image",
		"NAME":         `a: "b"`,
	}
	if err := a.FillEnv(vars); err != nil {
		t.Fatal(err)
	}
	if a.Env["DATABASE_URL"] != vars["DATABASE_URL"] || a.Env["STRIPE_KEY"] != vars["STRIPE_KEY"] || a.Env["GREETING"] != `hello a: "b", a: "b"` {
		t.Errorf("env = %q", a.Env)
	}
	if a.Domain != "api.example.com" || a.Image != "ghcr.io/team/orders-api" || len(a.Env) != 4 {
		t.Errorf("filling changed more than values: %+v", a)
	}
}

func TestFillEnvRefusesMissingVariables(t *testing.T) {
	a, _ := Decode([]byte(template))
	err := a.FillEnv(map[string]string{"NAME": "x"})
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL, STRIPE_KEY") {
		t.Errorf("FillEnv() = %v; want both missing names", err)
	}
	// An empty value is a value.
	a, _ = Decode([]byte(template))
	if err := a.FillEnv(map[string]string{"NAME": "", "DATABASE_URL": "", "STRIPE_KEY": ""}); err != nil {
		t.Errorf("FillEnv() with empty values = %v", err)
	}
}

func TestFieldsIgnoreHowTheFileIsWritten(t *testing.T) {
	a, _ := Decode([]byte("name: web\nimage: web\ndomain: w.example.com\nport: 80\nenv: {A: x, B: y}\nsecrets: [S1, S2]\n"))
	b, _ := Decode([]byte("# comment\nport: 80\nsecrets:\n  - S2\n  - S1\nenv:\n  B: 'y'\n  A: \"x\"\ndomain: w.example.com\nimage: web\nname: web\n"))
	if !bytes.Equal(Canonical(a.Fields()), Canonical(b.Fields())) {
		t.Errorf("fields differ:\n%s\n%s", Canonical(a.Fields()), Canonical(b.Fields()))
	}
	b.Env["A"] = "changed"
	if bytes.Equal(Canonical(a.Fields()), Canonical(b.Fields())) {
		t.Error("a changed value did not change the canonical form")
	}
	if !slices.Equal(slices.Sorted(maps.Keys(a.Fields())), []string{"domain", "env.A", "env.B", "image", "name", "port", "secrets"}) {
		t.Errorf("fields = %v", a.Fields())
	}
}
