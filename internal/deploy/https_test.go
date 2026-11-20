package deploy

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeCerts hands out certificates by marking the domain in the proxy as
// having one, which is what files appearing in cert_dir do for Nginx.
type fakeCerts struct {
	proxy *fakeProxy
	asked []string
	err   error
}

func (c *fakeCerts) Has(domain string) bool { return c.proxy.tls[domain] }

func (c *fakeCerts) Obtain(_ context.Context, domain string, progress func(string, ...any)) error {
	c.asked = append(c.asked, domain)
	progress("ordering a certificate for %s", domain)
	if c.err != nil {
		return c.err
	}
	if c.proxy.tls == nil {
		c.proxy.tls = map[string]bool{}
	}
	c.proxy.tls[domain] = true
	return nil
}

func TestDeploySwitchesANewDomainToHTTPS(t *testing.T) {
	fake, proxy := withPreviousRelease()
	certs := &fakeCerts{proxy: proxy}
	var out bytes.Buffer
	d := newTestDeployer(fake, &out)
	d.Proxy, d.Certs = proxy, certs

	if _, err := d.Deploy(context.Background(), testApp(), Options{}); err != nil {
		t.Fatalf("Deploy() error = %v\n%s", err, out.String())
	}
	if len(certs.asked) != 1 || certs.asked[0] != "api.example.com" {
		t.Errorf("certificates asked for = %v, want api.example.com once", certs.asked)
	}
	if live := proxy.live["orders-api"]; !strings.Contains(live, "release r2;") || !strings.Contains(live, "listen 443 ssl;") {
		t.Errorf("live config = %q, want r2 with HTTPS", live)
	}
	for _, want := range []string{"==> HTTPS for api.example.com", "    ordering a certificate for api.example.com", "https://api.example.com is on"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not contain %q:\n%s", want, out.String())
		}
	}
}

func TestDeployDoesNotAskAgainForADomainWithACertificate(t *testing.T) {
	fake, proxy := withPreviousRelease()
	proxy.tls = map[string]bool{"api.example.com": true}
	certs := &fakeCerts{proxy: proxy}
	var out bytes.Buffer
	d := newTestDeployer(fake, &out)
	d.Proxy, d.Certs = proxy, certs

	if _, err := d.Deploy(context.Background(), testApp(), Options{}); err != nil {
		t.Fatal(err)
	}
	if len(certs.asked) != 0 || strings.Contains(out.String(), "HTTPS for") {
		t.Errorf("asked for %v although a certificate exists:\n%s", certs.asked, out.String())
	}
	// The config the pipeline wrote already has HTTPS.
	if !strings.Contains(proxy.live["orders-api"], "listen 443 ssl;") {
		t.Errorf("live config = %q, want HTTPS", proxy.live["orders-api"])
	}
}

func TestDeploySucceedsWhenNoCertificateCanBeHad(t *testing.T) {
	fake, proxy := withPreviousRelease()
	certs := &fakeCerts{proxy: proxy, err: errors.New("the CA could not verify api.example.com: connection refused")}
	var out bytes.Buffer
	d := newTestDeployer(fake, &out)
	d.Proxy, d.Certs = proxy, certs

	if _, err := d.Deploy(context.Background(), testApp(), Options{}); err != nil {
		t.Fatalf("Deploy() error = %v; a missing certificate must not fail the deploy", err)
	}
	// The new release is in service, over HTTP.
	if live := proxy.live["orders-api"]; !strings.Contains(live, "release r2;") || strings.Contains(live, "443") {
		t.Errorf("live config = %q, want r2 without HTTPS", live)
	}
	if state := fake.State(testApp().Name + "-r2"); state != "running" {
		t.Errorf("the new container is %q, want running", state)
	}
	for _, want := range []string{"HTTPS is not on: the CA could not verify", "served over plain HTTP"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not contain %q:\n%s", want, out.String())
		}
	}
}

func TestDeployPutsBackTheConfigWhenNginxRejectsHTTPS(t *testing.T) {
	fake, proxy := withPreviousRelease()
	certs := &fakeCerts{proxy: proxy}
	var out bytes.Buffer
	d := newTestDeployer(fake, &out)
	d.Proxy, d.Certs = proxy, certs

	// Nginx accepts the pipeline's config, then rejects the HTTPS one.
	tests := 0
	d.Proxy = &failNthTest{fakeProxy: proxy, n: 2, count: &tests}

	if _, err := d.Deploy(context.Background(), testApp(), Options{}); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}
	if conf := proxy.files["orders-api"]; strings.Contains(conf, "443") || !strings.Contains(conf, "release r2;") {
		t.Errorf("config on disk = %q, want the HTTP config of r2 back", conf)
	}
	if !strings.Contains(out.String(), "HTTPS is not on: ssl_certificate") {
		t.Errorf("output does not say why:\n%s", out.String())
	}
}

// failNthTest fails the n-th config test.
type failNthTest struct {
	*fakeProxy
	n     int
	count *int
}

func (p *failNthTest) Test(ctx context.Context) error {
	*p.count++
	if *p.count == p.n {
		return errors.New(`ssl_certificate "/etc/liman/certs/api.example.com/fullchain.pem" failed`)
	}
	return p.fakeProxy.Test(ctx)
}

func TestDeployLeavesLocalhostDomainsAndDryRunsAlone(t *testing.T) {
	fake, proxy := withPreviousRelease()
	certs := &fakeCerts{proxy: proxy}
	var out bytes.Buffer
	d := newTestDeployer(fake, &out)
	d.Proxy, d.Certs = proxy, certs

	local := testApp()
	local.Domain = "orders.localhost"
	if _, err := d.Deploy(context.Background(), local, Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Deploy(context.Background(), testApp(), Options{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(certs.asked) != 0 {
		t.Errorf("certificates asked for = %v, want none", certs.asked)
	}
}
