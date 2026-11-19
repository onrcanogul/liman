package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// A middleware is a function that takes a handler and returns a handler. The
// returned one does something before and/or after calling the original.
// Because the input and output have the same type, middlewares stack.

// authenticate rejects requests that do not carry a valid token.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The health check must work without credentials: monitoring and
		// "is the daemon up?" checks have none.
		if r.URL.Path == "/v1/health" {
			next.ServeHTTP(w, r)
			return
		}

		client := clientAddress(r)
		limiter := s.authFailures()

		// Checked before the token is even looked at: during a lockout the
		// client learns nothing, not even whether a guess was right.
		if wait, locked := limiter.blocked(client); locked {
			s.noteRejection(false)
			w.Header().Set("Retry-After", retryAfter(wait))
			writeError(w, http.StatusTooManyRequests, tooManyAttempts(wait))
			return
		}

		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			limiter.recordFailure(client)
			// WWW-Authenticate tells the client which scheme is expected.
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, errors.New("missing bearer token"))
			return
		}

		valid, err := s.Store.VerifyToken(r.Context(), token)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("verify token: %w", err))
			return
		}
		if !valid {
			limiter.recordFailure(client)
			// The answer is the same whether the token is malformed,
			// unknown or revoked: an attacker learns nothing from it.
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, errors.New("invalid token"))
			return
		}

		next.ServeHTTP(w, r)
	})
}

// logRequests writes one line per request once it has finished.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		// Requests from a locked-out address are summarised elsewhere
		// (see rejections): a client hammering the API while locked out
		// must not be able to fill the disk with log lines.
		if rec.status == http.StatusTooManyRequests {
			return
		}
		// Successful health checks are not logged either. They carry no
		// information, monitoring sends them constantly, and because the
		// endpoint needs no token they are the one request anybody who can
		// reach the daemon could use to flood the log.
		if r.URL.Path == "/v1/health" && rec.status == http.StatusOK {
			return
		}

		// The path is logged, the query string and headers are not: the
		// Authorization header is a secret and must never reach a log.
		s.logger().Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(started).Round(time.Millisecond).String(),
			"remote", r.RemoteAddr,
		)
	})
}

// recoverPanics turns a panic in a handler into a 500 response. A bug in
// one request must not take the whole daemon down.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A deferred function runs even when the code after it panics, and
		// recover inside it stops the panic from propagating further.
		defer func() {
			if v := recover(); v != nil {
				s.logger().Error("panic in handler", "path", r.URL.Path, "panic", fmt.Sprint(v))
				// If the response has already started this write is too
				// late and has no effect; the connection is simply cut.
				writeError(w, http.StatusInternalServerError, errors.New("internal error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder remembers the status code a handler sent.
//
// It embeds the original ResponseWriter, so every method not defined here
// (Header, Write) is forwarded to it automatically.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.ResponseController find the original ResponseWriter.
// Without it, wrapping would hide the connection's Flush method and
// streaming would silently turn into buffering.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
