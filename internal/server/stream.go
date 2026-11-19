package server

import (
	"encoding/json"
	"net/http"

	"github.com/onrcanogul/liman/internal/api"
)

// eventStream sends DeployEvents to the client as they happen: one JSON
// object per line (the NDJSON format), flushed after every event.
//
// It is only ever used from the goroutine that handles the deploy request,
// so it needs no locking.
type eventStream struct {
	enc *json.Encoder
	rc  *http.ResponseController
}

func newEventStream(w http.ResponseWriter) *eventStream {
	w.Header().Set("Content-Type", "application/x-ndjson")
	// Tells proxies in between (Nginx honours this header) not to collect
	// the response before passing it on.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	s := &eventStream{
		// Encode writes the value followed by a newline, which is exactly
		// the line framing NDJSON needs.
		enc: json.NewEncoder(w),
		// ResponseController reaches the Flush method of the underlying
		// connection even when the ResponseWriter has been wrapped by
		// middleware, as long as the wrapper has an Unwrap method.
		rc: http.NewResponseController(w),
	}
	// Send the headers now, so the client knows the request was accepted
	// before the first step produces output.
	_ = s.rc.Flush()
	return s
}

// send writes one event and pushes it onto the network immediately.
//
// Errors are ignored on purpose: they mean the client has disconnected, and
// the deploy must carry on (or roll back) regardless. The request context
// is how the handler learns about the disconnect.
func (s *eventStream) send(ev api.DeployEvent) {
	_ = s.enc.Encode(ev)
	// Without the flush the bytes would sit in a buffer until it fills or
	// the handler returns, and the client would see nothing for seconds.
	_ = s.rc.Flush()
}

// Write makes eventStream an io.Writer: whatever the deployer prints becomes
// an output event. This is how the deploy package, which knows nothing
// about HTTP, ends up streaming over it.
func (s *eventStream) Write(p []byte) (int, error) {
	s.send(api.DeployEvent{Type: api.EventOutput, Text: string(p)})
	return len(p), nil
}

// flushWriter forwards writes to an HTTP response and flushes after each
// one. It is used for streaming plain text (logs).
type flushWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil {
		return n, err
	}
	return n, f.rc.Flush()
}
