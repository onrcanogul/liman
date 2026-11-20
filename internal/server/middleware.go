package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/onrcanogul/liman/internal/store"
)

// A middleware is a function that takes a handler and returns a handler. The
// returned one does something before and/or after calling the original.
// Because the input and output have the same type, middlewares stack.

// requestMeta is what later middleware learns about a request and earlier
// middleware wants to log. logRequests creates it and puts a pointer into
// the request's context; authenticate fills it in. A pointer, because a
// context is immutable: values can be added on the way in, but nothing can
// be passed back out except through something both sides can reach.
type requestMeta struct {
	// token is the name of the token the request was made with.
	token string
	scope string
	// expires is when that token stops working; zero for never.
	expires time.Time
}

// metaKey is the context key for *requestMeta. An unexported type of its
// own means no other package can produce the same key by accident.
type metaKey struct{}

func metaOf(ctx context.Context) *requestMeta {
	if m, ok := ctx.Value(metaKey{}).(*requestMeta); ok {
		return m
	}
	// Without logRequests in front (it always is) there is still something
	// to write into.
	return &requestMeta{}
}

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

		// The token is looked at first, and a valid one always gets
		// through. The limiter used to come first, which let anybody who
		// could send ten requests without a token (a web page with ten
		// <img> tags pointing here is enough) lock the real operator out
		// for as long as they kept it up: a limit meant to slow an
		// attacker down had become a switch for turning the owner off.
		//
		// Only something shaped like a token is looked up at all, so a
		// flood of junk costs no database query.
		token, hasToken := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		var info store.TokenInfo
		valid := false
		if hasToken && strings.HasPrefix(token, store.TokenPrefix) {
			var err error
			if info, valid, err = s.Store.LookupToken(r.Context(), token); err != nil {
				writeError(w, http.StatusInternalServerError, fmt.Errorf("verify token: %w", err))
				return
			}
		}

		if !valid {
			// During a lockout the client learns nothing more, and its
			// attempts are no longer counted or logged one by one.
			if wait, locked := limiter.blocked(client); locked {
				s.noteRejection(false)
				w.Header().Set("Retry-After", retryAfter(wait))
				writeError(w, http.StatusTooManyRequests, tooManyAttempts(wait))
				return
			}
			limiter.recordFailure(client)
			// WWW-Authenticate tells the client which scheme is expected.
			// The answer is the same whether the token is malformed,
			// unknown or revoked: an attacker learns nothing from it.
			w.Header().Set("WWW-Authenticate", "Bearer")
			switch {
			case !hasToken:
				writeError(w, http.StatusUnauthorized, errors.New("missing bearer token"))
			case info.Name != "":
				// LookupToken names a token it refuses only when that token
				// has expired. This one case does get its own answer: it
				// can only be told by somebody who holds a real token, the
				// token no longer opens anything, and "expired" is the one
				// word that saves its owner from hunting for a revocation
				// that never happened.
				s.logger().Warn("expired token used",
					"token", info.Name,
					"expired_at", info.ExpiresAt.UTC().Format(time.RFC3339),
					"remote", r.RemoteAddr)
				writeError(w, http.StatusUnauthorized, errors.New("token expired"))
			default:
				writeError(w, http.StatusUnauthorized, errors.New("invalid token"))
			}
			return
		}

		meta := metaOf(r.Context())
		meta.token, meta.scope, meta.expires = info.Name, info.Scope, info.ExpiresAt
		s.recordUse(r.Context(), info)

		// A read-only token may look and nothing else. The rule is about
		// the method, not a list of paths: an endpoint added later is
		// covered without anybody remembering to add it here, as long as
		// it follows HTTP (GET never changes anything, and it does not).
		if info.Scope != store.ScopeAdmin && r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusForbidden, errors.New("this token is read-only; the request needs a token created without --read-only"))
			return
		}

		next.ServeHTTP(w, r)
	})
}

// tokenUseInterval is how often a token's last use is written down. Writing
// it on every request made the slowest requests 60 times slower in a trial:
// the database has one connection, and every request queues behind a write
// that waits for the disk. "Used in the last minute" is all anybody needs
// to know before revoking a token.
const tokenUseInterval = time.Minute

// recordUse writes down that a token was accepted, at most once per
// tokenUseInterval. A failure is logged and otherwise ignored: the request
// itself is fine, and it must not fail over bookkeeping.
func (s *Server) recordUse(ctx context.Context, info store.TokenInfo) {
	now := time.Now()
	// The creation time is part of the key: a token revoked and created
	// again under the same name is a different token, and its first use
	// is written at once.
	key := info.Name + "\x00" + info.CreatedAt.Format(time.RFC3339Nano)
	if !s.used.firstIn(key, tokenUseInterval, now) {
		return
	}
	// WithoutCancel: a client that hangs up straight away was still
	// accepted, and its use is still recorded.
	if err := s.Store.TouchToken(context.WithoutCancel(ctx), info.Name, now); err != nil {
		s.logger().Warn("could not record token use", "token", info.Name, "err", err)
	}
}

// logRequests writes one line per request once it has finished.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		meta := &requestMeta{}

		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), metaKey{}, meta)))

		// Requests from a locked-out address are summarised elsewhere
		// (see rejections): a client hammering the API while locked out
		// must not be able to fill the disk with log lines.
		if rec.status == http.StatusTooManyRequests {
			return
		}
		// The same goes for requests refused because they came from a web
		// page: they are turned away before authentication, so anybody can
		// send as many as they like.
		if rec.status == http.StatusForbidden && meta.token == "" {
			s.noteRejection(false)
			return
		}
		// Successful health checks are not logged either. They carry no
		// information, monitoring sends them constantly, and because the
		// endpoint needs no token they are the one request anybody who can
		// reach the daemon could use to flood the log.
		if r.URL.Path == "/v1/health" && rec.status == http.StatusOK {
			return
		}
		// A successful read that the same token made of the same path a
		// little while ago is not logged again. The panel refreshes every
		// few seconds, and line after identical line would bury what the
		// log is for. Changes, failures and the first read of anything
		// still get their line, so "who looked at what, when" survives.
		path := truncate(r.URL.Path, 200)
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && rec.status < 300 && meta.token != "" &&
			!s.reads.firstIn(meta.token+" "+r.Method+" "+path, readLogWindow, started) {
			return
		}

		// The path is logged, the query string and headers are not: the
		// Authorization header is a secret and must never reach a log.
		s.logger().Info("request",
			"method", r.Method,
			// Cut off: the path is chosen by the client and may be as long
			// as the header limit allows.
			"path", path,
			"status", rec.status,
			"duration", time.Since(started).Round(time.Millisecond).String(),
			"remote", r.RemoteAddr,
			// Which token did it: the name, never the token. Empty for a
			// request that was turned away before it was authenticated.
			"token", meta.token,
		)
	})
}

// readLogWindow is how long a successful read is not logged again.
const readLogWindow = 10 * time.Minute

// maxReadLogKeys bounds readLog. Only successful reads are recorded, and
// those can only name what exists, so the table stays small; the bound is
// there all the same, because the paths come from the client.
const maxReadLogKeys = 4096

// readLog remembers when a read was last logged.
type readLog struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// firstIn reports whether key was not logged within window before now,
// and if so records now as the time it was. A key logged at 10:00 is
// therefore logged again at the first read from 10:10 on, however often
// it is read in between.
func (l *readLog) firstIn(key string, window time.Duration, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if at, ok := l.last[key]; ok && now.Sub(at) < window {
		return false
	}
	if l.last == nil {
		l.last = make(map[string]time.Time)
	}
	if len(l.last) >= maxReadLogKeys {
		for k, at := range l.last {
			if now.Sub(at) >= window {
				delete(l.last, k)
			}
		}
		// Still full: forget everything. The cost is some lines logged
		// again, never a line that is missing.
		if len(l.last) >= maxReadLogKeys {
			clear(l.last)
		}
	}
	l.last[key] = now
	return true
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

// truncate shortens s to at most n bytes for a log line.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
