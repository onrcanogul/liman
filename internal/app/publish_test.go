package app

import (
	"strings"
	"testing"
)

func TestPublishForms(t *testing.T) {
	tests := []struct {
		yaml, want string
	}{
		{"publish: 8080", "127.0.0.1:8080"},
		{"publish: 10.8.0.1:8080", "10.8.0.1:8080"},
		{`publish: "[fd00::1]:8080"`, "[fd00::1]:8080"},
		{"publish: [8080, 8081]", "127.0.0.1:8080,127.0.0.1:8081"},
		{"publish:\n  - 10.8.0.1:8080\n  - 10.8.0.1:8081", "10.8.0.1:8080,10.8.0.1:8081"},
	}
	for _, tt := range tests {
		a, err := Parse([]byte("name: api\nimage: a:1\nport: 3000\n" + tt.yaml + "\n"))
		if err != nil {
			t.Errorf("%s: %v", tt.yaml, err)
			continue
		}
		if got := a.Publish.String(); got != tt.want {
			t.Errorf("%s: Publish = %s, want %s", tt.yaml, got, tt.want)
		}
		// Back from the text a registration stores.
		if again, err := ParsePublish(a.Publish.String()); err != nil || again.String() != tt.want {
			t.Errorf("ParsePublish(%s) = %v, %v", tt.want, again, err)
		}
	}
}

func TestPublishRules(t *testing.T) {
	tests := []struct {
		name, yaml, wantErr string
	}{
		{"a name, not an address", "port: 3000\npublish: lb.example.com:8080", "must be an IP address"},
		{"port zero", "port: 3000\npublish: 0", "between 1 and 65535"},
		{"three", "port: 3000\npublish: [8080, 8081, 8082]", "at most two"},
		{"twice the same", "port: 3000\npublish: [8080, 127.0.0.1:8080]", "the same"},
		{"overlap on one port", "port: 3000\npublish: 8080\ndeploy:\n  strategy: overlap", "cannot hold two releases"},
		{"worker", "kind: worker\npublish: 8080", "a worker listens on no port"},
		{"neither domain nor publish", "port: 3000", "needs a domain, or publish"},
		{"a map", "port: 3000\npublish: {port: 8080}", "a port, an address:port, or a list"},
	}
	for _, tt := range tests {
		_, err := Parse([]byte("name: api\nimage: a:1\n" + tt.yaml + "\n"))
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: error = %v, want %q", tt.name, err, tt.wantErr)
		}
	}
}

func TestPublishStrategyAndCheck(t *testing.T) {
	one := &App{Publish: Publish{{Port: 8080}}}
	two := &App{Publish: Publish{{Port: 8080}, {Port: 8081}}}
	if one.Strategy() != StrategyReplace || two.Strategy() != StrategyOverlap {
		t.Errorf("strategies: one port %s, two ports %s", one.Strategy(), two.Strategy())
	}
	if (&App{Publish: Publish{{Host: "0.0.0.0", Port: 80}}}).Publish[0].Public() != true ||
		(&App{Publish: Publish{{Host: "10.8.0.1", Port: 80}}}).Publish[0].Public() != false ||
		one.Publish[0].Public() {
		t.Error("Public() is wrong")
	}
	// A domain and a fixed port together are fine.
	if _, err := Parse([]byte("name: api\nimage: a:1\nport: 3000\ndomain: api.example.com\npublish: 8080\n")); err != nil {
		t.Error(err)
	}
	f := two.Fields()
	if f["publish"] != "127.0.0.1:8080,127.0.0.1:8081" {
		t.Errorf("Fields()[publish] = %q", f["publish"])
	}
}

func TestOverlaps(t *testing.T) {
	ep := func(s string) Endpoint { e, _ := ParseEndpoint(s); return e }
	for _, tt := range []struct {
		a, b string
		want bool
	}{
		{"8080", "127.0.0.1:8080", true},
		{"8080", "8081", false},
		{"10.8.0.1:8080", "10.8.0.2:8080", false},
		{"0.0.0.0:8080", "10.8.0.1:8080", true},
		{"10.8.0.1:8080", "[::]:8080", true},
	} {
		if got := ep(tt.a).Overlaps(ep(tt.b)); got != tt.want {
			t.Errorf("%s overlaps %s = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}
