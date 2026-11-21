// Package health checks whether an app answers HTTP requests.
//
// Both the deploy (is the new release ready?) and the monitor (is it still
// working?) ask this question, so the answer lives in one place.
package health

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

// client is shared by all probes. An http.Client is safe for concurrent
// use and keeps its connections for reuse, so one is enough.
var client = &http.Client{
	Timeout: 3 * time.Second,
	// A health endpoint that redirects is answering, which is all a probe
	// needs to know. Following the redirect would test some other URL.
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// Probe performs one GET and treats any 2xx or 3xx response as healthy.
func Probe(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	return nil
}

// URL is the address of an app's health check: path on hostIP:hostPort,
// where the container is published. An empty hostIP is 127.0.0.1; an app
// published on another address is not reachable there.
func URL(hostIP string, hostPort int, path string) string {
	if hostIP == "" || hostIP == "0.0.0.0" || hostIP == "::" {
		// Published on every address: the loopback one is among them.
		hostIP = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(hostIP, strconv.Itoa(hostPort)) + path
}
