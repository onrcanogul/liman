package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAppendAndRead(t *testing.T) {
	// The nested directory does not exist yet; Append must create it.
	store := Store{Path: filepath.Join(t.TempDir(), "state", "history.jsonl")}

	first := Event{
		Time:       time.Date(2026, 11, 19, 18, 0, 0, 0, time.UTC),
		App:        "web",
		Release:    "20261119-180000",
		Step:       "Pull image web:1.0",
		Action:     "run",
		OK:         true,
		DurationMS: 1200,
	}
	second := Event{
		Time:    first.Time.Add(2 * time.Second),
		App:     "web",
		Release: "20261119-180000",
		Step:    "Start container",
		Action:  "run",
		Error:   "port is already allocated",
	}

	for _, ev := range []Event{first, second} {
		if err := store.Append(ev); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}

	got, err := store.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Read() returned %d events, want 2", len(got))
	}
	// Event contains only comparable fields, so == compares all of them.
	// time.Time needs Equal because two values can denote the same instant
	// with different internal representations.
	if !got[0].Time.Equal(first.Time) {
		t.Errorf("first time = %v, want %v", got[0].Time, first.Time)
	}
	got[0].Time, got[1].Time = first.Time, second.Time
	if got[0] != first {
		t.Errorf("first event = %+v, want %+v", got[0], first)
	}
	if got[1] != second {
		t.Errorf("second event = %+v, want %+v", got[1], second)
	}
}

func TestReadMissingFileIsEmpty(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "nope.jsonl")}

	got, err := store.Read()

	if err != nil || len(got) != 0 {
		t.Errorf("Read() = %v, %v; want no events and no error", got, err)
	}
}

func TestReadReportsCorruptLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(path, []byte("{\"app\":\"web\"}\nnot json\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Store{Path: path}.Read()

	if err == nil {
		t.Fatal("Read() error = nil, want an error for the corrupt line")
	}
}

func TestHistoryFileIsPrivate(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "history.jsonl")}
	if err := store.Append(Event{App: "web"}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	info, err := os.Stat(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600", perm)
	}
}
