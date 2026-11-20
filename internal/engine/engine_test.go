package engine

import (
	"strings"
	"testing"
)

func TestWithDigest(t *testing.T) {
	d := "sha256:" + strings.Repeat("ab", 32)
	tests := []struct {
		image, want string
	}{
		{"ghcr.io/user/api:main", "ghcr.io/user/api@" + d},
		{"ghcr.io/user/api", "ghcr.io/user/api@" + d},
		{"nginx:1.27-alpine", "nginx@" + d},
		// The port of a registry is not a tag.
		{"localhost:5055/demo/api:main", "localhost:5055/demo/api@" + d},
		{"localhost:5055/demo/api", "localhost:5055/demo/api@" + d},
		// A digest already there is replaced, tag and all.
		{"ghcr.io/user/api:main@sha256:" + strings.Repeat("cd", 32), "ghcr.io/user/api@" + d},
	}
	for _, tt := range tests {
		got, err := WithDigest(tt.image, d)
		if err != nil || got != tt.want {
			t.Errorf("WithDigest(%q) = %q, %v; want %q", tt.image, got, err, tt.want)
		}
	}
}

func TestValidDigest(t *testing.T) {
	good := "sha256:" + strings.Repeat("0f", 32)
	for _, d := range []string{
		"", "sha256:xyz", "sha256:" + strings.Repeat("0", 63), "sha256:" + strings.Repeat("0", 65),
		"sha256:" + strings.Repeat("AB", 32), "sha512:" + strings.Repeat("00", 32), good + "\n", "x" + good,
	} {
		if ValidDigest(d) {
			t.Errorf("ValidDigest(%q) = true", d)
		}
		if _, err := WithDigest("api:main", d); err == nil {
			t.Errorf("WithDigest with digest %q succeeded", d)
		}
	}
	if !ValidDigest(good) {
		t.Errorf("ValidDigest(%q) = false", good)
	}
}
