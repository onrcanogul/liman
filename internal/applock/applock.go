// Package applock makes sure that only one thing at a time changes an app.
//
// A deploy, a stop and an automatic restart all replace or restart the
// containers of an app. Two of them running at once would each act on a
// state the other is in the middle of changing.
package applock

import "sync"

// Locks holds the set of apps that are currently being changed. The zero
// value is ready to use. It must not be copied after first use.
type Locks struct {
	// mu guards busy. The users of this type run in different goroutines:
	// HTTP handlers and the health monitor.
	mu   sync.Mutex
	busy map[string]bool
}

// TryLock marks app as busy. If it already is, TryLock returns false at
// once; it never waits. Otherwise it returns the function that marks the
// app as free again, which may be called more than once.
//
// Refusing instead of waiting is deliberate. A caller that waited would
// later act on whatever the previous holder left behind, which is not the
// state it was asked to act on.
func (l *Locks) TryLock(app string) (unlock func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.busy[app] {
		return nil, false
	}
	// A nil map can be read but not written, so create it on first use.
	if l.busy == nil {
		l.busy = map[string]bool{}
	}
	l.busy[app] = true

	// OnceFunc makes the release idempotent: callers can both defer it and
	// call it early without freeing an app that somebody else has locked in
	// the meantime.
	return sync.OnceFunc(func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		delete(l.busy, app)
	}), true
}
