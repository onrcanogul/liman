package docker

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/onrcanogul/liman/internal/engine"
)

func TestFilesArchive(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	data, err := filesArchive([]engine.File{
		{Path: "/run/secrets/DATABASE_URL", Content: []byte("postgres://x"), Mode: 0o444},
		{Path: "/run/secrets/TLS_KEY", Content: []byte("-----BEGIN-----"), Mode: 0o444},
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	type entry struct {
		name     string
		dir      bool
		mode     int64
		uid, gid int
		content  string
	}
	var got []entry
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		if !h.ModTime.Equal(now) {
			t.Errorf("%s: mtime %v, want %v", h.Name, h.ModTime, now)
		}
		if len(h.PAXRecords) != 0 && h.PAXRecords["SCHILY.xattr.com.apple.provenance"] != "" {
			t.Errorf("%s carries an xattr", h.Name)
		}
		got = append(got, entry{h.Name, h.Typeflag == tar.TypeDir, h.Mode, h.Uid, h.Gid, string(body)})
	}
	want := []entry{
		{"run/", true, 0o755, 0, 0, ""},
		{"run/secrets/", true, 0o755, 0, 0, ""},
		{"run/secrets/DATABASE_URL", false, 0o444, 0, 0, "postgres://x"},
		{"run/secrets/TLS_KEY", false, 0o444, 0, 0, "-----BEGIN-----"},
	}
	if len(got) != len(want) {
		t.Fatalf("archive = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestFilesArchiveRejectsOddPaths(t *testing.T) {
	for _, p := range []string{"run/secrets/X", "/run/../etc/passwd", "/", "/run/secrets/"} {
		if _, err := filesArchive([]engine.File{{Path: p, Mode: 0o444}}, time.Now()); err == nil {
			t.Errorf("filesArchive accepted %q", p)
		}
	}
}
