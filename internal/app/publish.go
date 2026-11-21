package app

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Endpoint is a fixed address on the server an app is published on.
type Endpoint struct {
	// Host is an IP address; "" means 127.0.0.1.
	Host string
	Port int
}

// DefaultPublishHost is where a port is published when no address is given:
// reachable from the server itself (a proxy liman does not drive, a tunnel),
// from nowhere else.
const DefaultPublishHost = "127.0.0.1"

// Address is the endpoint as host:port, with the default host filled in.
func (e Endpoint) Address() string {
	host := e.Host
	if host == "" {
		host = DefaultPublishHost
	}
	return net.JoinHostPort(host, strconv.Itoa(e.Port))
}

// IP is the endpoint's host with the default filled in.
func (e Endpoint) IP() string {
	if e.Host == "" {
		return DefaultPublishHost
	}
	return e.Host
}

// Public reports whether the endpoint can be reached from beyond the server
// and its private networks: an unspecified address (0.0.0.0, ::) or a
// public one. Docker's published ports pass by ufw, so such an endpoint is
// on the internet whatever the firewall says.
func (e Endpoint) Public() bool {
	ip, err := netip.ParseAddr(e.IP())
	if err != nil {
		return true
	}
	return !ip.IsLoopback() && !ip.IsPrivate()
}

// Overlaps reports whether two endpoints claim the same port: on the same
// address, or one of them on every address (0.0.0.0, ::).
func (e Endpoint) Overlaps(other Endpoint) bool {
	if e.Port != other.Port {
		return false
	}
	every := func(ip string) bool { return ip == "0.0.0.0" || ip == "::" }
	return e.IP() == other.IP() || every(e.IP()) || every(other.IP())
}

// Publish is the list of fixed endpoints of an app: one, or two that its
// releases take turns on. In YAML it is a port (8080), an address
// ("10.8.0.1:8080"), or a list of up to two of those.
type Publish []Endpoint

// UnmarshalYAML implements yaml.Unmarshaler.
func (p *Publish) UnmarshalYAML(node *yaml.Node) error {
	var items []*yaml.Node
	switch node.Kind {
	case yaml.ScalarNode:
		items = []*yaml.Node{node}
	case yaml.SequenceNode:
		items = node.Content
	default:
		return fmt.Errorf("line %d: publish is a port, an address:port, or a list of them", node.Line)
	}
	out := make(Publish, 0, len(items))
	for _, it := range items {
		if it.Kind != yaml.ScalarNode {
			return fmt.Errorf("line %d: publish is a port, an address:port, or a list of them", it.Line)
		}
		e, err := ParseEndpoint(it.Value)
		if err != nil {
			return fmt.Errorf("line %d: publish: %w", it.Line, err)
		}
		out = append(out, e)
	}
	*p = out
	return nil
}

// ParseEndpoint reads "8080", "10.8.0.1:8080" or "[fd00::1]:8080".
func ParseEndpoint(s string) (Endpoint, error) {
	host, portText := "", s
	if strings.ContainsAny(s, ":") {
		h, p, err := net.SplitHostPort(s)
		if err != nil {
			return Endpoint{}, fmt.Errorf("%q is not a port or an address:port", s)
		}
		ip, err := netip.ParseAddr(h)
		if err != nil {
			return Endpoint{}, fmt.Errorf("%q: the address must be an IP address, not a name", s)
		}
		host, portText = ip.String(), p
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return Endpoint{}, fmt.Errorf("%q: the port must be between 1 and 65535", s)
	}
	return Endpoint{Host: host, Port: port}, nil
}

// String is the canonical text of the list: "127.0.0.1:8080,127.0.0.1:8081".
// A registration stores it, and a definition must match it.
func (p Publish) String() string {
	parts := make([]string, len(p))
	for i, e := range p {
		parts[i] = e.Address()
	}
	return strings.Join(parts, ",")
}

// ParsePublish reads what String writes, and what limand app create takes.
func ParsePublish(s string) (Publish, error) {
	if s == "" {
		return nil, nil
	}
	var out Publish
	for _, part := range strings.Split(s, ",") {
		e, err := ParseEndpoint(strings.TrimSpace(part))
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}
