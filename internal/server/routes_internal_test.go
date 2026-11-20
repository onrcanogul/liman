package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// These tests are in package server because they look at the route table
// itself, which is not exported.

// Every endpoint says what it gives access to. A route registered without
// that decision would be one that tokens limited to some apps can reach
// by accident; the table is the only place routes come from, so checking
// the table checks the API.
func TestEveryRouteSaysWhatItGivesAccessTo(t *testing.T) {
	s := &Server{}
	seen := map[string]bool{}
	for _, rt := range s.routes() {
		if rt.access == accessUnset {
			t.Errorf("%s: no access given", rt.pattern)
		}
		if rt.handler == nil {
			t.Errorf("%s: no handler", rt.pattern)
		}
		if seen[rt.pattern] {
			t.Errorf("%s: listed twice", rt.pattern)
		}
		seen[rt.pattern] = true
	}
}

// A route that does say nothing is closed to limited tokens, and open to
// the rest as before.
func TestAnUnsetAccessIsClosedToLimitedTokens(t *testing.T) {
	s := &Server{}
	called := false
	h := s.guard(accessUnset, func(http.ResponseWriter, *http.Request) { called = true })

	for _, tc := range []struct {
		apps   []string
		status int
		called bool
	}{
		{nil, http.StatusOK, true},
		{[]string{"orders-api"}, http.StatusForbidden, false},
	} {
		called = false
		req := httptest.NewRequest(http.MethodGet, "/v1/added-later", nil)
		req = req.WithContext(context.WithValue(req.Context(), metaKey{}, &requestMeta{token: "t", apps: tc.apps}))
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != tc.status || called != tc.called {
			t.Errorf("apps %v: status %d, handler called %v; want %d, %v", tc.apps, rec.Code, called, tc.status, tc.called)
		}
	}
}
