//go:build integration

package proxy_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onrcanogul/liman/internal/config"
	"github.com/onrcanogul/liman/internal/proxy"
	"github.com/onrcanogul/liman/internal/proxy/caddy"
	"github.com/onrcanogul/liman/internal/proxy/nginx"
	"github.com/onrcanogul/liman/internal/proxy/traefik"
)

// Each proxy runs for real, in a container, with liman's directory mounted
// where it reads it from. The test does what a deploy does: route a domain
// to one upstream, switch it to another while requests keep coming, and
// take it away again.

const domain = "web.localhost" // no public CA issues for it: plain HTTP everywhere

type proxyUnderTest struct {
	name    string
	dialect proxy.Dialect
	image   string
	// setup writes the proxy's own config into dir and returns the docker
	// run arguments that mount it and liman's directory, and the arguments
	// for the proxy itself.
	setup func(t *testing.T, dir, confDir string) (mounts []string, args []string)
	// commands are the test and reload commands, run in the container.
	commands func(container string) (test, reload []string)
}

var proxies = []proxyUnderTest{
	{
		name: "nginx", dialect: nginx.Dialect{}, image: "nginx:1.27-alpine",
		setup: func(t *testing.T, dir, confDir string) ([]string, []string) {
			// A default server that is not liman's, as on a real machine:
			// a domain nothing routes gets an answer without the header.
			conf := filepath.Join(dir, "conf.d")
			write(t, filepath.Join(conf, "default.conf"), "server { listen 80 default_server; return 404; }\n")
			write(t, filepath.Join(conf, "liman.conf"), "include /etc/nginx/liman.d/*.conf;\n")
			return []string{"-v", conf + ":/etc/nginx/conf.d:ro", "-v", confDir + ":/etc/nginx/liman.d:ro"}, nil
		},
		commands: func(c string) ([]string, []string) {
			return []string{"docker", "exec", c, "nginx", "-t"}, []string{"docker", "exec", c, "nginx", "-s", "reload"}
		},
	},
	{
		name: "caddy", dialect: caddy.Dialect{}, image: "caddy:2.11.9-alpine",
		setup: func(t *testing.T, dir, confDir string) ([]string, []string) {
			caddyfile := filepath.Join(dir, "Caddyfile")
			write(t, caddyfile, "import /etc/caddy/liman.d/*.caddy\n\n:80 {\n\trespond 404\n}\n")
			return []string{"-v", caddyfile + ":/etc/caddy/Caddyfile:ro", "-v", confDir + ":/etc/caddy/liman.d:ro"}, nil
		},
		commands: func(c string) ([]string, []string) {
			test, reload := caddy.Dialect{}.Commands()
			return append([]string{"docker", "exec", c}, test...), append([]string{"docker", "exec", c}, reload...)
		},
	},
	{
		name: "traefik", dialect: traefik.Dialect{}, image: "traefik:v3.7.19",
		setup: func(t *testing.T, _, confDir string) ([]string, []string) {
			return []string{"-v", confDir + ":/etc/traefik/liman.d:ro"},
				[]string{"--providers.file.directory=/etc/traefik/liman.d", "--providers.file.watch=true", "--entrypoints.web.address=:80"}
		},
		commands: func(string) ([]string, []string) { return nil, nil },
	},
}

func TestRealProxies(t *testing.T) {
	a, b := upstream(t, "A"), upstream(t, "B")

	for _, p := range proxies {
		t.Run(p.name, func(t *testing.T) {
			dir := t.TempDir()
			confDir := filepath.Join(dir, "liman.d")
			if err := os.MkdirAll(confDir, 0o755); err != nil {
				t.Fatal(err)
			}
			// The proxy in the container may run as another user.
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			mounts, args := p.setup(t, dir, confDir)

			container := fmt.Sprintf("liman-proxy-test-%s-%d", p.name, os.Getpid())
			_ = exec.Command("docker", "rm", "-f", container).Run()
			run := append([]string{"run", "-d", "--name", container, "-p", "127.0.0.1::80",
				// The upstreams listen on this machine.
				"--add-host", "host.docker.internal:host-gateway"}, mounts...)
			run = append(append(run, p.image), args...)
			docker(t, run...)
			t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", container).Run() })
			probe := strings.Split(docker(t, "port", container, "80/tcp"), "\n")[0]

			test, reload := p.commands(container)
			m := proxy.New(config.Proxy{
				ConfDir: confDir, UpstreamHost: "host.docker.internal", ProbeAddr: probe,
				TestCommand: test, ReloadCommand: reload,
			}, p.dialect)
			ctx := context.Background()

			// The proxy must be up and answering for nothing of ours.
			waitUp(t, probe)
			if err := m.WaitRelease(ctx, domain, ""); err != nil {
				t.Fatalf("before any route: %v", err)
			}

			route := func(release string, port int) {
				t.Helper()
				content, err := m.Render("web", domain, port, release)
				if err == nil {
					err = m.Write("web", content)
				}
				if err == nil {
					err = m.Test(ctx)
				}
				if err == nil {
					err = m.Reload(ctx)
				}
				if err == nil {
					err = m.WaitRelease(ctx, domain, release)
				}
				if err != nil {
					t.Fatalf("route %s: %v\n%s", release, err, docker(t, "logs", "--tail", "20", container))
				}
			}

			route("r1", a)
			if got := get(t, probe); got != "A" {
				t.Fatalf("after r1 the app answers %q, want A", got)
			}

			// The switch, with requests coming all the while: none may fail.
			stop := make(chan struct{})
			var wg sync.WaitGroup
			var mu sync.Mutex
			var answers []string
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					got := getOrErr(probe)
					mu.Lock()
					answers = append(answers, got)
					mu.Unlock()
				}
			}()
			time.Sleep(200 * time.Millisecond)
			route("r2", b)
			time.Sleep(200 * time.Millisecond)
			close(stop)
			wg.Wait()

			seen := map[string]int{}
			for _, got := range answers {
				seen[got]++
			}
			if len(seen) > 2 || seen["A"] == 0 || seen["B"] == 0 {
				t.Errorf("answers during the switch = %v; want only A, then B", seen)
			}
			if answers[len(answers)-1] != "B" {
				t.Errorf("last answer %q, want B", answers[len(answers)-1])
			}
			t.Logf("%s: %d requests during the switch, %v", p.name, len(answers), seen)

			// Undo: no file, no route.
			if err := m.Restore("web", nil); err != nil {
				t.Fatal(err)
			}
			if err := m.Test(ctx); err != nil {
				t.Fatal(err)
			}
			if err := m.Reload(ctx); err != nil {
				t.Fatal(err)
			}
			if err := m.WaitRelease(ctx, domain, ""); err != nil {
				t.Errorf("after removing the route: %v", err)
			}
		})
	}
}

// upstream starts an HTTP server answering body, reachable from containers.
func upstream(t *testing.T, body string) int {
	t.Helper()
	// Not httptest.NewServer: it listens on 127.0.0.1 only, which a
	// container does not reach on Linux.
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return l.Addr().(*net.TCPAddr).Port
}

func get(t *testing.T, probe string) string {
	t.Helper()
	got := getOrErr(probe)
	if strings.HasPrefix(got, "error") || strings.HasPrefix(got, "status") {
		t.Fatalf("GET %s: %s", domain, got)
	}
	return got
}

// getOrErr returns the body, or what went wrong.
func getOrErr(probe string) string {
	req, _ := http.NewRequest(http.MethodGet, "http://"+probe+"/", nil)
	req.Host = domain
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "error: " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("status %d", resp.StatusCode)
	}
	return string(body)
}

func waitUp(t *testing.T, probe string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", probe, time.Second); err == nil {
			_ = c.Close()
			// Listening is not yet serving for every proxy.
			time.Sleep(300 * time.Millisecond)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("nothing listens on %s", probe)
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
