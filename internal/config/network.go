package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
)

// The API is plain HTTP: the token and everything else travel unencrypted,
// and whoever holds a token controls Docker on the server. Encryption and
// reachability are therefore delegated to the network the daemon listens
// on: the loopback interface, or a private network such as WireGuard.
//
// The two checks below turn that rule from documentation into code. Both
// sides enforce it: the daemon refuses to listen on a public address, and
// the client refuses to send its token to one in clear text.

// cgnat is 100.64.0.0/10, the "shared address space". It is not a public
// range; mesh VPNs such as Tailscale hand out addresses from it.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// isPrivate reports whether addr can only be reached from this machine or
// from a private network.
func isPrivate(addr netip.Addr) bool {
	// An IPv4 address written in IPv6 form (::ffff:10.0.0.1) must be judged
	// as the IPv4 address it is.
	addr = addr.Unmap()
	return addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || cgnat.Contains(addr)
}

// CheckListenAddress verifies that the daemon may listen on address.
func CheckListenAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("must have the form host:port: %w", err)
	}

	// An empty host means "every interface", exactly like 0.0.0.0.
	if host == "" {
		return errors.New("must name one address; an empty host listens on every interface, public ones included")
	}

	// A host name is refused on purpose: what it resolves to is decided by
	// DNS at start-up, outside of this file's control.
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("host must be an IP address, got %q", host)
	}
	if addr.IsUnspecified() {
		return fmt.Errorf("%s listens on every interface, public ones included; name the loopback or private address instead", host)
	}
	if !isPrivate(addr) {
		return fmt.Errorf("%s is a public address; the API is unencrypted and must only listen on loopback or a private network (such as WireGuard)", host)
	}
	return nil
}

// CheckClientURL verifies that the client may send its token to rawURL.
func CheckClientURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		// url.Parse rejects some inputs outright, for example "10.8.0.1:7070"
		// without a scheme. The likeliest cause is the missing prefix.
		return fmt.Errorf("URL must start with http:// or https://, got %q", rawURL)
	}

	switch u.Scheme {
	case "https":
		// Encrypted and authenticated by TLS; any host is fine.
		return nil
	case "http":
	default:
		return fmt.Errorf("URL must start with http:// or https://, got %q", rawURL)
	}

	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("refusing to send the token over unencrypted http to host name %q; use its private IP address or https", host)
	}
	if !isPrivate(addr) {
		return fmt.Errorf("refusing to send the token over unencrypted http to the public address %s; use a private network (such as WireGuard) or https", host)
	}
	return nil
}
