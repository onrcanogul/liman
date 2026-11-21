package traefik

import (
	"testing"

	"github.com/onrcanogul/liman/internal/proxy"
	"github.com/onrcanogul/liman/internal/proxy/proxytest"
	"go.yaml.in/yaml/v3"
)

func TestContract(t *testing.T) {
	proxytest.Contract(t, Dialect{})
}

func TestRender(t *testing.T) {
	got, err := Dialect{}.Render(proxy.Site{App: "web", Domain: "web.example.com", Upstream: "127.0.0.1:40001", Release: "r2"})
	if err != nil {
		t.Fatal(err)
	}
	want := "# Managed by liman for app \"web\".\n" +
		"# Do not edit: this file is overwritten on every deploy.\n" +
		"http:\n" +
		"  routers:\n" +
		"    liman-web:\n" +
		"      rule: \"Host(`web.example.com`)\"\n" +
		"      service: liman-web\n" +
		"      middlewares:\n" +
		"        - liman-web-release\n" +
		"  middlewares:\n" +
		"    liman-web-release:\n" +
		"      headers:\n" +
		"        customResponseHeaders:\n" +
		"          X-Liman-Release: \"r2\"\n" +
		"  services:\n" +
		"    liman-web:\n" +
		"      loadBalancer:\n" +
		"        servers:\n" +
		"          - url: \"http://127.0.0.1:40001\"\n"
	if string(got) != want {
		t.Errorf("Render() =\n%s\nwant\n%s", got, want)
	}
}

// A file Traefik cannot parse is not applied, and nothing says so but its
// log: the output must be YAML of the shape the file provider reads.
func TestRenderIsTraefiksYAML(t *testing.T) {
	out, err := Dialect{}.Render(proxy.Site{App: "orders-api", Domain: "api.example.com", Upstream: "127.0.0.1:<free port>", Release: "20261121-101500-123"})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		HTTP struct {
			Routers map[string]struct {
				Rule        string   `yaml:"rule"`
				Service     string   `yaml:"service"`
				Middlewares []string `yaml:"middlewares"`
			} `yaml:"routers"`
			Middlewares map[string]struct {
				Headers struct {
					CustomResponseHeaders map[string]string `yaml:"customResponseHeaders"`
				} `yaml:"headers"`
			} `yaml:"middlewares"`
			Services map[string]struct {
				LoadBalancer struct {
					Servers []struct {
						URL string `yaml:"url"`
					} `yaml:"servers"`
				} `yaml:"loadBalancer"`
			} `yaml:"services"`
		} `yaml:"http"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("not YAML: %v\n%s", err, out)
	}
	r := doc.HTTP.Routers["liman-orders-api"]
	if r.Rule != "Host(`api.example.com`)" || r.Service != "liman-orders-api" || len(r.Middlewares) != 1 {
		t.Errorf("router = %+v", r)
	}
	if got := doc.HTTP.Middlewares[r.Middlewares[0]].Headers.CustomResponseHeaders[proxy.ReleaseHeader]; got != "20261121-101500-123" {
		t.Errorf("release header = %q", got)
	}
	if s := doc.HTTP.Services[r.Service].LoadBalancer.Servers; len(s) != 1 || s[0].URL != "http://127.0.0.1:<free port>" {
		t.Errorf("servers = %+v", s)
	}
}
