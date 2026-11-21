package certs

import "testing"

func TestIssuable(t *testing.T) {
	for domain, want := range map[string]bool{
		"api.example.com":    true,
		"example.com":        true,
		"web.localhost":      false,
		"localhost":          false,
		"whoami.test":        false,
		"shop.example":       false,
		"printer.local":      false,
		"nas.home.arpa":      false,
		"wiki.corp.internal": false,
		"testing.com":        true, // only the label "test" is reserved
		"":                   false,
		"Not A Domain":       false,
	} {
		if got := Issuable(domain); got != want {
			t.Errorf("Issuable(%q) = %v, want %v", domain, got, want)
		}
	}
}
