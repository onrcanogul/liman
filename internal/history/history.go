// Package history keeps an append-only record of every step liman executed.
//
// The record is a JSON Lines file: one JSON object per line. Appending a
// line never rewrites earlier ones, and a half-written last line (after a
// crash) does not corrupt the rest. Stage 5 moves this into SQLite.
package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Event is one executed step.
//
// The json tags define the names used in the file. omitempty leaves a field
// out when it has its zero value.
type Event struct {
	Time    time.Time `json:"time"`
	App     string    `json:"app"`
	Release string    `json:"release"`
	Step    string    `json:"step"`
	// Action is "run" or "undo".
	Action string `json:"action"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	// DurationMS is how long the step took, in milliseconds.
	DurationMS int64 `json:"duration_ms"`
}

// Store reads and writes the history file at Path.
type Store struct {
	Path string
}

// Append adds one event to the end of the file, creating the file and its
// directory if needed.
func (s Store) Append(ev Event) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return fmt.Errorf("create history directory: %w", err)
	}

	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("encode history event: %w", err)
	}

	// O_APPEND makes the operating system put every write at the end of
	// the file, even if two liman processes write at the same time.
	// 0o600 means only the owner may read or write the file.
	f, err := os.OpenFile(s.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open history: %w", err)
	}

	// Write the line and its newline in a single call so a concurrent
	// writer cannot land in between.
	_, writeErr := f.Write(append(line, '\n'))
	// Close can fail too (for example when the disk is full), and for a
	// file that was written to that failure matters.
	closeErr := f.Close()

	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("write history: %w", err)
	}
	return nil
}

// Read returns all events, oldest first. A missing file is an empty history,
// not an error.
func (s Store) Read() ([]Event, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read history: %w", err)
	}

	var events []Event
	// Scanner splits the input into lines without loading them all into
	// separate strings up front.
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for line := 1; scanner.Scan(); line++ {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			return nil, fmt.Errorf("history line %d: %w", line, err)
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read history: %w", err)
	}
	return events, nil
}
