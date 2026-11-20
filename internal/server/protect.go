package server

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

// This file holds the defences that are not about what a request asks for,
// but about how requests arrive: too slowly, too many at once, from a
// browser, or with guessed credentials.

// Defaults for the limits below. They are generous for a tool with one
// operator and tight enough that abuse hits them quickly.
const (
	defaultMaxInFlight     = 64
	defaultBodyReadTimeout = 15 * time.Second

	// A client gets this many failed authentications per window before it
	// is locked out for the rest of the window.
	maxAuthFailures   = 10
	authFailureWindow = time.Minute

	// maxTrackedClients bounds the limiter's own memory.
	maxTrackedClients = 10_000
)

// --- failed authentication limiter ----------------------------------------

// authLimiter counts failed authentications per client address and locks an
// address out once it has too many.
//
// Tokens are 256 random bits, so guessing one is hopeless even without this.
// The limiter is defence in depth: it makes guessing loud (a 429 in the log
// instead of a silent stream of 401s) and it bounds the work an
// unauthenticated client can make the daemon do.
type authLimiter struct {
	// mu guards failures. Requests are handled concurrently.
	mu       sync.Mutex
	failures map[string]*failureWindow

	// now is the clock; tests replace it to move time forward instantly.
	now func() time.Time
}

type failureWindow struct {
	count int
	// resets is when the window ends and the count starts over.
	resets time.Time
}

func newAuthLimiter() *authLimiter {
	return &authLimiter{failures: map[string]*failureWindow{}, now: time.Now}
}

// blocked reports whether client is locked out and, if so, for how long.
func (l *authLimiter) blocked(client string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	w, ok := l.failures[client]
	if !ok {
		return 0, false
	}
	now := l.now()
	if !now.Before(w.resets) {
		// The window is over: forget it.
		delete(l.failures, client)
		return 0, false
	}
	if w.count < maxAuthFailures {
		return 0, false
	}
	return w.resets.Sub(now), true
}

// recordFailure notes one failed authentication by client.
func (l *authLimiter) recordFailure(client string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()

	// An attacker can make up as many source addresses as the network lets
	// them, and each one costs an entry. Dropping expired entries whenever
	// the map has grown keeps the limiter itself from becoming the way to
	// exhaust the daemon's memory.
	if len(l.failures) >= maxTrackedClients {
		for addr, w := range l.failures {
			if !now.Before(w.resets) {
				delete(l.failures, addr)
			}
		}
	}

	w, ok := l.failures[client]
	// Still full after the clean-up above: every entry is live. A new
	// address is then not tracked at all. That lets it guess without being
	// slowed down, which costs nothing (the tokens cannot be guessed); the
	// alternative, growing without bound, would let an attacker with many
	// addresses take the daemon's memory.
	if !ok && len(l.failures) >= maxTrackedClients {
		return
	}
	if !ok || !now.Before(w.resets) {
		l.failures[client] = &failureWindow{count: 1, resets: now.Add(authFailureWindow)}
		return
	}
	w.count++
}

// clientAddress returns the address a request came from, without the port.
//
// Only the address of the actual TCP connection is used. Headers such as
// X-Forwarded-For are written by the client and can say anything; trusting
// them would let an attacker pick a fresh "address" for every guess.
func clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	// One IPv6 customer is normally handed a whole /64: 2^64 addresses to
	// pick a fresh one from for every attempt. They are counted as the one
	// client they are.
	if addr, err := netip.ParseAddr(host); err == nil && addr.Is6() && !addr.Is4In6() {
		if prefix, err := addr.Prefix(64); err == nil {
			return prefix.String()
		}
	}
	return host
}

// --- logging of rejected requests ---------------------------------------------

// rejections counts requests that were turned away wholesale (shed under
// load, or from a locked-out address) and reports them as a summary, at
// most once per interval.
//
// Logging each of them individually hands an attacker a second weapon: the
// load test wrote 192 MB of log in 36 seconds. A flood of rejected requests
// must cost one log line per second, not one per request.
type rejections struct {
	mu        sync.Mutex
	shed      int
	lockedOut int
	lastLog   time.Time
}

const rejectionLogInterval = time.Second

// note counts one rejected request and returns the totals to log, if it is
// time to log them.
func (r *rejections) note(shed bool, now time.Time) (shedCount, lockedOutCount int, logNow bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if shed {
		r.shed++
	} else {
		r.lockedOut++
	}
	if now.Sub(r.lastLog) < rejectionLogInterval {
		return 0, 0, false
	}

	shedCount, lockedOutCount = r.shed, r.lockedOut
	r.shed, r.lockedOut, r.lastLog = 0, 0, now
	return shedCount, lockedOutCount, true
}

// noteRejection records a rejected request and writes the summary line when
// one is due.
func (s *Server) noteRejection(shed bool) {
	if shedCount, lockedOut, logNow := s.rejected.note(shed, time.Now()); logNow {
		s.logger().Warn("requests rejected",
			"shed_under_load", shedCount,
			"from_locked_out_addresses", lockedOut,
		)
	}
}

// --- middleware -----------------------------------------------------------

// limitConcurrency rejects requests while too many are already being
// handled. Every request costs a goroutine, memory and often a Docker or
// database call; without a cap, opening connections faster than they finish
// is enough to exhaust the daemon.
func (s *Server) limitConcurrency(next http.Handler) http.Handler {
	limit := s.MaxInFlight
	if limit == 0 {
		limit = defaultMaxInFlight
	}
	// A buffered channel used as a counting semaphore: sending takes a slot
	// and blocks (here: fails) when all slots are taken; receiving frees one.
	slots := make(chan struct{}, limit)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			next.ServeHTTP(w, r)
		default:
			s.noteRejection(true)
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusServiceUnavailable, errors.New("the daemon is handling too many requests; try again shortly"))
		}
	})
}

// secureHeaders sets response headers that tell clients and intermediaries
// how to treat the answers.
func (s *Server) secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// Responses describe infrastructure and may contain log output;
		// nothing between the daemon and the client should keep a copy.
		h.Set("Cache-Control", "no-store")
		// Stops a browser from guessing a content type and, say, rendering
		// log output as HTML.
		h.Set("X-Content-Type-Options", "nosniff")
		// Other sites may not load anything from here, not even as an
		// image or a script whose content they cannot read: the mere fact
		// that a request succeeded, or how long it took, tells them things.
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// rejectBrowsers refuses requests made by web pages other than the daemon's
// own panel.
//
// The daemon usually listens on 127.0.0.1, and any web page open in a
// browser on that machine can send requests there. Such a page cannot add
// the Authorization header to a simple request, so the token already
// protects the API; this check removes the remaining surface (for example
// probing unauthenticated endpoints) for free. Browsers attach an Origin
// header to every cross-origin request, and to same-origin ones that are
// not a plain GET.
//
// The one browser client that is welcome is the panel, and its requests
// are recognisable: their Origin is the daemon's own address.
func (s *Server) rejectBrowsers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Not every request a page can cause carries an Origin header: an
		// <img> or <script> tag pointing here sends a plain GET without
		// one. Browsers label those too, with Sec-Fetch-Site. "same-origin"
		// is the panel, "none" is an address typed by the user, and no
		// header at all is a client that is not a browser.
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			writeError(w, http.StatusForbidden, errors.New("requests from web pages are not accepted"))
			return
		}

		origin := r.Header.Get("Origin")
		if origin == "" || (s.Panel != nil && sameOrigin(origin, r.Host)) {
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, http.StatusForbidden, errors.New("requests from web pages are not accepted"))
	})
}

// sameOrigin reports whether a request with this Origin header, sent to
// this Host, comes from a page the daemon itself served.
//
// Comparing the two is not enough on its own. Both are chosen by the
// browser from the address the user's page was loaded from, and a hostile
// site can make its own name resolve to 127.0.0.1 ("DNS rebinding"): its
// page then really is same-origin with the daemon, under the attacker's
// name. The daemon is only ever addressed by IP address or as localhost,
// so any other host name is refused. (Such a page would still have no
// token; this closes the door before that matters.)
func sameOrigin(origin, host string) bool {
	if origin != "http://"+host {
		return false
	}
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	if name == "localhost" {
		return true
	}
	_, err := netip.ParseAddr(name)
	return err == nil
}

// --- request bodies -------------------------------------------------------

// readBody reads a request body of at most limit bytes, giving the client a
// bounded time to send it.
//
// Two different attacks are covered: a body that is too large (limit) and a
// body that arrives one byte at a time to keep a connection and a goroutine
// occupied indefinitely (deadline).
func (s *Server) readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	timeout := s.BodyReadTimeout
	if timeout == 0 {
		timeout = defaultBodyReadTimeout
	}
	rc := http.NewResponseController(w)

	// The deadline applies to this connection's reads only. The server-wide
	// ReadTimeout is not used, because it would stay armed after the body
	// has been read: when it fired, the server would treat the connection
	// as broken and cancel the request context, aborting a long deploy that
	// was working fine. Setting the deadline here and clearing it below
	// limits the upload without limiting the work that follows.
	_ = rc.SetReadDeadline(time.Now().Add(timeout))

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		// Before it writes a response, Go's server tries to read whatever
		// is left of the request body, so the connection can be reused for
		// the next request. For a client that stalls on purpose that read
		// would never end, and the response would never be sent: the
		// connection would stay tied up after all. "Connection: close"
		// tells the server not to bother and to drop the connection once
		// it has answered. The deadline is left in place for the same
		// reason. (Found by TestSlowRequestBodyIsCutOff.)
		w.Header().Set("Connection", "close")
		return nil, err
	}

	// The body is in. Clear the deadline so it cannot fire during the work
	// that follows.
	_ = rc.SetReadDeadline(time.Time{})
	return body, nil
}

// bodyErrorStatus picks the status code for a failed readBody.
func bodyErrorStatus(err error) int {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return http.StatusRequestTimeout
	}
	return http.StatusBadRequest
}

// retryAfter formats a duration for the Retry-After header, which takes
// whole seconds. It rounds up so the client never retries too early.
func retryAfter(d time.Duration) string {
	seconds := int(d / time.Second)
	if d%time.Second != 0 {
		seconds++
	}
	return strconv.Itoa(max(seconds, 1))
}

func tooManyAttempts(wait time.Duration) error {
	return fmt.Errorf("too many failed authentication attempts; try again in %s", wait.Round(time.Second))
}
