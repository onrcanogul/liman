package main

import (
	"strings"
	"testing"

	"github.com/onrcanogul/liman/internal/config"
	"github.com/onrcanogul/liman/internal/docker"
)

func TestEngineOptions(t *testing.T) {
	for _, tt := range []struct {
		cfg     config.Engine
		name    string
		host    string
		libpod  bool
		wantErr string
	}{
		{cfg: config.Engine{}, name: "Docker", host: ""},
		{cfg: config.Engine{Driver: "docker", Socket: "/var/run/docker.sock"}, name: "Docker", host: "unix:///var/run/docker.sock"},
		{cfg: config.Engine{Driver: "podman"}, name: "Podman", host: "unix:///run/podman/podman.sock", libpod: true},
		{cfg: config.Engine{Driver: "containerd"}, wantErr: `unknown engine "containerd"`},
	} {
		got, err := engineOptions(tt.cfg)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("%+v: error = %v, want %q", tt.cfg, err, tt.wantErr)
			}
			continue
		}
		if err != nil || got.Name != tt.name || got.Host != tt.host || (got.RegistryCheck == docker.CheckRegistry) != tt.libpod {
			t.Errorf("%+v: %+v, %v", tt.cfg, got, err)
		}
	}
}
