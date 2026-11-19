package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/onrcanogul/liman/internal/engine/enginetest"
	"github.com/onrcanogul/liman/internal/pipeline"
)

// fakeProxy keeps one config per app in memory. "live" is what a reload
// last applied, which is what real traffic would follow.
type fakeProxy struct {
	calls []string
	files map[string]string
	live  map[string]string

	testErr   error
	reloadErr error
}

func newFakeProxy() *fakeProxy {
	return &fakeProxy{files: map[string]string{}, live: map[string]string{}}
}

func (p *fakeProxy) Path(app string) string { return "/etc/nginx/liman/" + app + ".conf" }
func (p *fakeProxy) TestCommand() string    { return "nginx -t" }
func (p *fakeProxy) ReloadCommand() string  { return "nginx -s reload" }

func (p *fakeProxy) Render(_, domain string, hostPort int, release string) ([]byte, error) {
	port := "<free port>"
	if hostPort != 0 {
		port = fmt.Sprint(hostPort)
	}
	return fmt.Appendf(nil, "release %s;\nserver_name %s;\nproxy_pass 127.0.0.1:%s;\n", release, domain, port), nil
}

// ReleaseOf reads back the first line written by Render.
func (p *fakeProxy) ReleaseOf(content []byte) string {
	first, _, _ := strings.Cut(string(content), ";")
	return strings.TrimPrefix(first, "release ")
}

// WaitRelease fails unless the live config, the one a reload applied,
// belongs to the release. That is exactly the property the real method
// observes over HTTP.
func (p *fakeProxy) WaitRelease(_ context.Context, domain, release string) error {
	p.calls = append(p.calls, "wait "+release)
	for _, content := range p.live {
		if strings.Contains(content, "server_name "+domain+";") {
			if got := p.ReleaseOf([]byte(content)); got != release {
				return fmt.Errorf("proxy serves release %q, want %q", got, release)
			}
			return nil
		}
	}
	if release != "" {
		return fmt.Errorf("proxy does not serve %s", domain)
	}
	return nil
}

func (p *fakeProxy) Current(app string) ([]byte, error) {
	content, ok := p.files[app]
	if !ok {
		return nil, nil
	}
	return []byte(content), nil
}

func (p *fakeProxy) Write(app string, content []byte) error {
	p.calls = append(p.calls, "write")
	p.files[app] = string(content)
	return nil
}

func (p *fakeProxy) Restore(app string, content []byte) error {
	p.calls = append(p.calls, "restore")
	if content == nil {
		delete(p.files, app)
	} else {
		p.files[app] = string(content)
	}
	return nil
}

func (p *fakeProxy) Test(context.Context) error {
	p.calls = append(p.calls, "test")
	return p.testErr
}

func (p *fakeProxy) Reload(context.Context) error {
	p.calls = append(p.calls, "reload")
	if p.reloadErr != nil {
		return p.reloadErr
	}
	// A reload makes whatever is on disk live.
	p.live = map[string]string{}
	for app, content := range p.files {
		p.live[app] = content
	}
	return nil
}

const (
	oldConf = "release r1;\nserver_name api.example.com;\nproxy_pass 127.0.0.1:40000;\n"
	newConf = "release r2;\nserver_name api.example.com;\nproxy_pass 127.0.0.1:40001;\n"
)

// withPreviousRelease returns an engine and a proxy in the state a
// successful earlier deploy leaves behind: r1 running on port 40000 and
// Nginx pointing at it.
func withPreviousRelease() (*enginetest.Fake, *fakeProxy) {
	proxy := newFakeProxy()
	proxy.files["orders-api"] = oldConf
	proxy.live["orders-api"] = oldConf
	return withPrevious(), proxy
}

func TestDeployWithProxySwitchesTrafficBeforeRemovingPrevious(t *testing.T) {
	fake, proxy := withPreviousRelease()
	var out bytes.Buffer
	d := newTestDeployer(fake, &out)
	d.Proxy = proxy

	// Record the order of everything that happened across both fakes.
	var events []string
	opts := Options{OnEvent: func(_ string, ev pipeline.Event) {
		events = append(events, ev.Step)
	}}

	if _, err := d.Deploy(context.Background(), testApp(), opts); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	wantSteps := []string{
		"Pull image " + testImage,
		"Allocate a free host port",
		"Start container " + newContainer,
		"Health check",
		"Write Nginx config for api.example.com",
		"Reload Nginx",
		"Retire previous containers of orders-api",
	}
	if !slices.Equal(events, wantSteps) {
		t.Errorf("steps = %v, want %v", events, wantSteps)
	}

	if want := []string{"write", "test", "reload", "wait r2"}; !slices.Equal(proxy.calls, want) {
		t.Errorf("proxy calls = %v, want %v", proxy.calls, want)
	}
	if proxy.live["orders-api"] != newConf {
		t.Errorf("live config = %q, want it to point at the new container", proxy.live["orders-api"])
	}

	// The diff must be visible in a normal run, not only in a dry run.
	for _, want := range []string{"    - proxy_pass 127.0.0.1:40000;\n", "    + proxy_pass 127.0.0.1:40001;\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not contain the diff line %q:\n%s", want, out.String())
		}
	}
}

func TestDeployWithProxyFailureRestoresRouting(t *testing.T) {
	tests := []struct {
		name  string
		setup func(p *fakeProxy)
		opts  Options
		// wantProxyCalls is everything the proxy saw, including the undo.
		wantProxyCalls []string
	}{
		{
			name:  "config test fails",
			setup: func(p *fakeProxy) { p.testErr = errors.New("nginx: [emerg] unexpected \"}\"") },
			// The reload step failed before reloading, so only the write is
			// undone and Nginx is never reloaded.
			wantProxyCalls: []string{"write", "test", "restore"},
		},
		{
			name:           "reload fails",
			setup:          func(p *fakeProxy) { p.reloadErr = errors.New("nginx is not running") },
			wantProxyCalls: []string{"write", "test", "reload", "restore"},
		},
		{
			name: "user declines the removal of the previous release",
			opts: Options{Confirm: func(step pipeline.Step) (bool, error) {
				return !strings.HasPrefix(step.Name, "Retire previous"), nil
			}},
			// Traffic had already been switched, so the undo of the reload
			// step restores the file and reloads; the undo of the write
			// step then restores it once more.
			wantProxyCalls: []string{"write", "test", "reload", "wait r2", "restore", "test", "reload", "wait r1", "restore"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake, proxy := withPreviousRelease()
			if tt.setup != nil {
				tt.setup(proxy)
			}
			d := newTestDeployer(fake, io.Discard)
			d.Proxy = proxy

			_, err := d.Deploy(context.Background(), testApp(), tt.opts)
			if err == nil {
				t.Fatal("Deploy() error = nil, want a failure")
			}

			if !slices.Equal(proxy.calls, tt.wantProxyCalls) {
				t.Errorf("proxy calls = %v, want %v", proxy.calls, tt.wantProxyCalls)
			}

			// The invariant: after a failed deploy the file on disk, the
			// live routing and the running container are all the old ones.
			if proxy.files["orders-api"] != oldConf {
				t.Errorf("config on disk = %q, want the previous config", proxy.files["orders-api"])
			}
			if proxy.live["orders-api"] != oldConf {
				t.Errorf("live config = %q, want the previous config", proxy.live["orders-api"])
			}
			if _, ok := fake.Containers[prevContainer]; !ok || len(fake.Containers) != 1 {
				t.Errorf("containers = %v, want only the previous one", fake.Containers)
			}
		})
	}
}

func TestFirstDeployWithProxyFailureRemovesConfig(t *testing.T) {
	fake, proxy := enginetest.New(), newFakeProxy()
	proxy.testErr = errors.New("nginx: [emerg] host not found in upstream")
	d := newTestDeployer(fake, io.Discard)
	d.Proxy = proxy

	if _, err := d.Deploy(context.Background(), testApp(), Options{}); err == nil {
		t.Fatal("Deploy() error = nil, want a failure")
	}

	// There was no config before, so none may be left behind: a stale file
	// would break the next reload for every app.
	if _, exists := proxy.files["orders-api"]; exists {
		t.Error("a config file was left behind after a failed first deploy")
	}
	if len(fake.Containers) != 0 {
		t.Errorf("%d containers left, want none", len(fake.Containers))
	}
}

func TestDryRunWithProxyShowsDiffAndChangesNothing(t *testing.T) {
	_, proxy := withPreviousRelease()
	var out bytes.Buffer
	d := newTestDeployer(nil, &out)
	d.Proxy = proxy

	if _, err := d.Deploy(context.Background(), testApp(), Options{DryRun: true}); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	if len(proxy.calls) != 0 {
		t.Errorf("proxy calls during dry run = %v, want none", proxy.calls)
	}

	want := `==> [5/7] Write Nginx config for api.example.com
    $ $EDITOR /etc/nginx/liman/orders-api.conf
    - release r1;
    + release r2;
      server_name api.example.com;
    - proxy_pass 127.0.0.1:40000;
    + proxy_pass 127.0.0.1:<free port>;
==> [6/7] Reload Nginx
    $ nginx -t && nginx -s reload
==> [7/7] Retire previous containers of orders-api
`
	if !strings.Contains(out.String(), want) {
		t.Errorf("output does not contain\n%s\ngot\n%s", want, out.String())
	}
}

func TestDeployFailsWhenProxyNeverSwitches(t *testing.T) {
	fake, proxy := withPreviousRelease()
	d := newTestDeployer(fake, io.Discard)
	// stuckProxy accepts reloads but keeps serving what it served before,
	// like an Nginx whose reload silently failed.
	d.Proxy = &stuckProxy{fakeProxy: proxy}

	_, err := d.Deploy(context.Background(), testApp(), Options{})

	if err == nil || !strings.Contains(err.Error(), `Reload Nginx: proxy serves release "r1", want "r2"`) {
		t.Fatalf("Deploy() error = %v, want the reload step to fail", err)
	}
	// The old container must survive: it is still the one receiving traffic.
	if _, ok := fake.Containers[prevContainer]; !ok || len(fake.Containers) != 1 {
		t.Errorf("containers = %v, want only the previous one", fake.Containers)
	}
	if proxy.files["orders-api"] != oldConf {
		t.Errorf("config on disk = %q, want the previous config", proxy.files["orders-api"])
	}
}

// stuckProxy embeds *fakeProxy: it has all of its methods, and overrides
// one. Embedding is how Go composes types; there is no inheritance.
type stuckProxy struct {
	*fakeProxy
}

func (p *stuckProxy) Reload(context.Context) error {
	p.calls = append(p.calls, "reload")
	return nil // claims success, changes nothing
}
