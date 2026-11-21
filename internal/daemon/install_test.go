package daemon

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/onrcanogul/liman/internal/install"
)

func TestTermAsker(t *testing.T) {
	var out bytes.Buffer
	a := &termAsker{in: bufio.NewReader(strings.NewReader("\n7\ncaddy\n\n8.8.8.8\nmaybe\nn\n")), out: &out}
	options := []install.Option{{Value: "nginx", Label: "on port 80"}, {Value: "caddy"}, {Value: "none"}}

	// Enter takes the default; a number out of range is asked again; a
	// name is an answer too.
	if i, err := a.Choose("Reverse proxy", options, 0); err != nil || i != 0 {
		t.Errorf("default: %d, %v", i, err)
	}
	if i, err := a.Choose("Reverse proxy", options, 0); err != nil || i != 1 {
		t.Errorf("7, then caddy: %d, %v", i, err)
	}
	if s, err := a.Text("API address", "10.8.0.1:7070"); err != nil || s != "10.8.0.1:7070" {
		t.Errorf("Text default: %q, %v", s, err)
	}
	if s, _ := a.Text("API address", ""); s != "8.8.8.8" {
		t.Errorf("Text: %q", s)
	}
	if yes, err := a.Confirm("Deploys from CI?", true); err != nil || yes {
		t.Errorf("maybe, then n: %v, %v", yes, err)
	}
	if !strings.Contains(out.String(), "*[1] nginx    on port 80") || !strings.Contains(out.String(), `"7" is none of them`) {
		t.Errorf("output:\n%s", out.String())
	}
	// No more input: an error, not a default taken silently.
	if _, err := a.Confirm("again?", true); err == nil {
		t.Error("an answer read from nothing")
	}
}

func TestInstallFlags(t *testing.T) {
	td := newTestDaemon(t)
	for _, args := range [][]string{
		{"install", "--no-nginx", "--proxy", "caddy"},
		{"install", "extra"},
	} {
		td.stderr.buf.Reset()
		if code := td.Run(context.Background(), args); code != exitUsage {
			t.Errorf("%v: code %d, stderr %s", args, code, td.stderr.String())
		}
	}
}
