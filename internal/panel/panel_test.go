package panel

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// get requests path from the panel handler and returns the response and
// its body.
func get(t *testing.T, method, path string) (*http.Response, string) {
	t.Helper()

	// httptest.NewRecorder is a ResponseWriter that keeps what a handler
	// writes; no network is involved.
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(method, path, nil))

	resp := rec.Result()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

func TestIndexIsServedWithAStrictPolicy(t *testing.T) {
	resp, body := get(t, http.MethodGet, "/")

	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("GET / = %d %q, want 200 and HTML", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(body, `<script src="app.js">`) {
		t.Error("the index does not load app.js")
	}

	policy := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(policy, want) {
			t.Errorf("Content-Security-Policy = %q, want it to contain %q", policy, want)
		}
	}
	// Either of these would take the teeth out of the policy.
	for _, bad := range []string{"unsafe-inline", "unsafe-eval", "*"} {
		if strings.Contains(policy, bad) {
			t.Errorf("Content-Security-Policy = %q, must not contain %q", policy, bad)
		}
	}
	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
}

func TestEveryFileTheIndexRefersToIsEmbedded(t *testing.T) {
	_, index := get(t, http.MethodGet, "/")

	refs := regexp.MustCompile(`(?:href|src)="([^"]+)"`).FindAllStringSubmatch(index, -1)
	if len(refs) < 3 {
		t.Fatalf("found %d references in the index, want the style sheet, the script and the icon", len(refs))
	}
	for _, ref := range refs {
		// The policy only allows files of the daemon itself.
		if strings.Contains(ref[1], ":") || strings.HasPrefix(ref[1], "/") {
			t.Errorf("the index refers to %q, want a relative path", ref[1])
			continue
		}
		if resp, _ := get(t, http.MethodGet, "/"+ref[1]); resp.StatusCode != http.StatusOK {
			t.Errorf("GET /%s = %d, want 200: the file is not embedded", ref[1], resp.StatusCode)
		}
	}
}

// The panel's promise is that data is never interpreted as markup or code.
// This test pins down the ways that promise is usually broken, so that a
// later change cannot break it quietly.
func TestPageNeverTurnsStringsIntoMarkupOrCode(t *testing.T) {
	_, script := get(t, http.MethodGet, "/app.js")
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "srcdoc"} {
		if strings.Contains(script, bad) {
			t.Errorf("app.js uses %s", bad)
		}
	}
	// Every request that changes something goes through send() or
	// stream(), with the method as the first argument. The list below is
	// everything the panel may do; a new use shows up here and has to be
	// thought about (does it need a confirmation? can it lose data?).
	calls := regexp.MustCompile(`\b(?:send|stream)\('([A-Z]+)', ([^,)]+)`).FindAllStringSubmatch(script, -1)
	var got []string
	for _, c := range calls {
		got = append(got, c[1]+" "+c[2])
	}
	want := []string{
		"POST '/v1/backups'",
		"POST '/v1/backups/' + encodeURIComponent(name",
		"POST appPath(a.name",
		"DELETE appPath(a.name",
		"PUT appPath(r.app",
		"DELETE appPath(a.name",
		"DELETE '/v1/tokens/' + encodeURIComponent(t.name",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests that change something:\n%s\nwant exactly:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// And nothing goes around them.
	for _, verb := range []string{"'POST'", "'PUT'", "'DELETE'", "'PATCH'"} {
		if n := strings.Count(script, verb); n != len(regexp.MustCompile(`(?:send|stream)\(`+verb).FindAllString(script, -1)) {
			t.Errorf("app.js names %s outside a send() or stream() call", verb)
		}
	}
	// A secret's value must not be kept anywhere but the request.
	for _, bad := range []string{"localStorage.setItem('liman-secret", "console.log"} {
		if strings.Contains(script, bad) {
			t.Errorf("app.js contains %s", bad)
		}
	}

	_, index := get(t, http.MethodGet, "/")
	if regexp.MustCompile(`<script[^>]*>[^<\s]`).MatchString(index) {
		t.Error("the index contains an inline script, which the policy would block")
	}
	if regexp.MustCompile(`\s(style|on[a-z]+)=`).MatchString(index) {
		t.Error("the index contains an inline style or event handler, which the policy would block")
	}
}

func TestOnlyReadingIsAllowed(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		if resp, _ := get(t, method, "/"); resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s / = %d, want 405", method, resp.StatusCode)
		}
	}
	if resp, _ := get(t, http.MethodGet, "/no-such-file.js"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET of a missing file = %d, want 404", resp.StatusCode)
	}
	// The embedded tree must not be reachable above its root.
	if resp, body := get(t, http.MethodGet, "/../panel.go"); resp.StatusCode == http.StatusOK && strings.Contains(body, "package panel") {
		t.Error("a file outside the static directory was served")
	}
}
