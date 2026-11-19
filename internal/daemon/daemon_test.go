package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onrcanogul/liman/internal/api"
	"github.com/onrcanogul/liman/internal/config"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/engine/enginetest"
	"github.com/onrcanogul/liman/internal/store"
)

// syncBuffer is a bytes.Buffer that may be written by one goroutine (the
// server) while another (the test) reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type testDaemon struct {
	*Daemon
	stdout *syncBuffer
	stderr *syncBuffer
	engine *enginetest.Fake
}

func newTestDaemon(t *testing.T) *testDaemon {
	t.Helper()

	td := &testDaemon{stdout: &syncBuffer{}, stderr: &syncBuffer{}, engine: enginetest.New()}
	td.Daemon = &Daemon{
		Stdout:          td.stdout,
		Stderr:          td.stderr,
		Home:            t.TempDir(),
		Connect:         func(context.Context) (engine.Engine, error) { return td.engine, nil },
		ShutdownTimeout: 5 * time.Second,
	}
	// Port 0 asks the operating system for any free port, so parallel test
	// runs never fight over one.
	writeConfig(t, td.Home, "daemon:\n  listen: 127.0.0.1:0\n")
	return td
}

func writeConfig(t *testing.T, home, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

var tokenRe = regexp.MustCompile(store.TokenPrefix + `[0-9a-f]{64}`)

func TestTokenLifecycle(t *testing.T) {
	td := newTestDaemon(t)
	ctx := context.Background()

	if code := td.Run(ctx, []string{"token", "list"}); code != exitOK || !strings.Contains(td.stdout.String(), "No tokens.") {
		t.Fatalf("token list on an empty store: code %d, output %q", code, td.stdout.String())
	}

	if code := td.Run(ctx, []string{"token", "create", "laptop"}); code != exitOK {
		t.Fatalf("token create: code %d, stderr %q", code, td.stderr.String())
	}
	token := tokenRe.FindString(td.stdout.String())
	if token == "" {
		t.Fatalf("token create did not print a token:\n%s", td.stdout.String())
	}

	// The printed token must be the one the store accepts.
	st, err := store.Open(ctx, filepath.Join(td.Home, "liman.db"))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.VerifyToken(ctx, token); !ok {
		t.Error("the printed token is not accepted")
	}
	_ = st.Close()

	if code := td.Run(ctx, []string{"token", "create", "laptop"}); code != exitError || !strings.Contains(td.stderr.String(), "already exists") {
		t.Errorf("creating a duplicate: code %d, stderr %q", code, td.stderr.String())
	}

	if code := td.Run(ctx, []string{"token", "list"}); code != exitOK || !strings.Contains(td.stdout.String(), "laptop") {
		t.Errorf("token list: code %d, output %q", code, td.stdout.String())
	}

	if code := td.Run(ctx, []string{"token", "revoke", "laptop"}); code != exitOK {
		t.Errorf("token revoke: code %d, stderr %q", code, td.stderr.String())
	}
	if code := td.Run(ctx, []string{"token", "revoke", "laptop"}); code != exitError {
		t.Errorf("revoking twice: code %d, want %d", code, exitError)
	}
}

func TestTokenCreateWriteClientDoesNotPrintTheToken(t *testing.T) {
	td := newTestDaemon(t)
	clientFile := filepath.Join(t.TempDir(), "client.yaml")

	code := td.Run(context.Background(), []string{"token", "create", "dev", "--write-client", clientFile, "--url", "http://10.8.0.1:7070"})
	if code != exitOK {
		t.Fatalf("code %d, stderr %q", code, td.stderr.String())
	}

	if tokenRe.MatchString(td.stdout.String()) || tokenRe.MatchString(td.stderr.String()) {
		t.Error("the token was printed although it was written to a file")
	}

	client, err := config.LoadClient(clientFile)
	if err != nil {
		t.Fatal(err)
	}
	if client.URL != "http://10.8.0.1:7070" || !tokenRe.MatchString(client.Token) {
		t.Errorf("client.yaml has url %q and a token of length %d", client.URL, len(client.Token))
	}
}

func TestUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"unknown command", []string{"nope"}, `unknown command "nope"`},
		{"token without subcommand", []string{"token"}, "usage: limand token"},
		{"token create without name", []string{"token", "create"}, "usage: limand token create"},
		{"token create with a flag as name", []string{"token", "create", "--url", "x"}, "usage: limand token create"},
		{"url without write-client", []string{"token", "create", "dev", "--url", "http://x"}, "--url only makes sense"},
		{"revoke without name", []string{"token", "revoke"}, "usage: limand token revoke"},
		{"serve with arguments", []string{"serve", "extra"}, "usage: limand serve"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			td := newTestDaemon(t)

			code := td.Run(context.Background(), tt.args)

			if code != exitUsage || !strings.Contains(td.stderr.String(), tt.want) {
				t.Errorf("code %d, stderr %q; want %d and %q", code, td.stderr.String(), exitUsage, tt.want)
			}
		})
	}
}

// startServing runs the daemon in the background and returns its address, a
// function that stops it, and a channel carrying its exit code.
func startServing(t *testing.T, td *testDaemon) (addr string, stop context.CancelFunc, exit <-chan int) {
	t.Helper()

	ready := make(chan string, 1)
	td.Ready = func(a string) { ready <- a }

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan int, 1)
	go func() { done <- td.Run(ctx, nil) }()

	select {
	case addr = <-ready:
	case code := <-done:
		t.Fatalf("the daemon exited with code %d before it was ready: %s", code, td.stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not become ready")
	}
	return addr, cancel, done
}

func TestServeAndShutDown(t *testing.T) {
	td := newTestDaemon(t)
	clientFile := filepath.Join(t.TempDir(), "client.yaml")
	if code := td.Run(context.Background(), []string{"token", "create", "dev", "--write-client", clientFile}); code != exitOK {
		t.Fatalf("token create: %s", td.stderr.String())
	}
	cfg, err := config.LoadClient(clientFile)
	if err != nil {
		t.Fatal(err)
	}

	addr, stop, exit := startServing(t, td)
	client := &api.Client{BaseURL: "http://" + addr, Token: cfg.Token}

	if _, err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	td.engine.Add(engine.Container{Name: "web-r1", App: "web"})
	apps, err := client.Apps(context.Background())
	if err != nil || len(apps) != 1 {
		t.Fatalf("Apps() = %v, %v; want one container", apps, err)
	}

	stop()

	select {
	case code := <-exit:
		if code != exitOK {
			t.Errorf("exit code after shutdown = %d, stderr: %s", code, td.stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not stop")
	}

	for _, want := range []string{"limand started", "msg=request", "path=/v1/apps", "status=200", "limand stopped"} {
		if !strings.Contains(td.stderr.String(), want) {
			t.Errorf("log does not contain %q:\n%s", want, td.stderr.String())
		}
	}
	// Successful health checks are deliberately absent from the log.
	if strings.Contains(td.stderr.String(), "path=/v1/health") {
		t.Errorf("the log contains health checks:\n%s", td.stderr.String())
	}
	// The token travels in a header; it must never appear in the log.
	if strings.Contains(td.stderr.String(), cfg.Token) {
		t.Error("the log contains the API token")
	}
}

func TestShutdownWaitsForRunningDeploy(t *testing.T) {
	td := newTestDaemon(t)
	clientFile := filepath.Join(t.TempDir(), "client.yaml")
	td.Run(context.Background(), []string{"token", "create", "dev", "--write-client", clientFile})
	cfg, _ := config.LoadClient(clientFile)

	addr, stop, exit := startServing(t, td)
	client := &api.Client{BaseURL: "http://" + addr, Token: cfg.Token}

	const definition = "name: web\nimage: web:1.0\ndomain: web.example.com\nport: 80\n"

	// A step-mode deploy that parks at its first question keeps a request
	// open for as long as the test wants.
	asked := make(chan struct{})
	answer := make(chan bool)
	var once sync.Once
	deployDone := make(chan error, 1)
	go func() {
		opts := api.DeployOptions{Confirm: func(string) (bool, error) {
			once.Do(func() { close(asked) })
			return <-answer, nil
		}}
		_, err := client.Deploy(context.Background(), []byte(definition), opts, io.Discard)
		deployDone <- err
	}()

	select {
	case <-asked:
	case <-time.After(10 * time.Second):
		t.Fatal("the deploy never asked its first question")
	}

	// Ask the daemon to stop while the deploy is still in progress.
	stop()

	select {
	case code := <-exit:
		t.Fatalf("the daemon exited (code %d) while a deploy was still running", code)
	case <-time.After(200 * time.Millisecond):
		// Still running, as it should be.
	}

	// Decline the step; the deploy ends and the daemon may now exit.
	answer <- false
	if err := <-deployDone; err == nil {
		t.Error("the declined deploy reported success")
	}

	select {
	case code := <-exit:
		if code != exitOK {
			t.Errorf("exit code = %d, stderr: %s", code, td.stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not stop after the deploy finished")
	}
}

func TestServeFailsWhenAddressIsTaken(t *testing.T) {
	first := newTestDaemon(t)
	addr, _, _ := startServing(t, first)

	second := newTestDaemon(t)
	writeConfig(t, second.Home, "daemon:\n  listen: "+addr+"\n")

	code := second.Run(context.Background(), nil)

	if code != exitError || !strings.Contains(second.stderr.String(), "listen on "+addr) {
		t.Errorf("code %d, stderr %q; want a listen error", code, second.stderr.String())
	}
}

func TestServeWarnsWithoutTokens(t *testing.T) {
	td := newTestDaemon(t)

	_, stop, exit := startServing(t, td)
	stop()
	<-exit

	if !strings.Contains(td.stderr.String(), "no API tokens exist yet") {
		t.Errorf("log does not warn about missing tokens:\n%s", td.stderr.String())
	}
}

func TestConnectOnceSharesOneConnection(t *testing.T) {
	calls := 0
	failures := 2
	fake := enginetest.New()

	connect := connectOnce(func(context.Context) (engine.Engine, error) {
		calls++
		if failures > 0 {
			failures--
			return nil, errors.New("docker is down")
		}
		return fake, nil
	})

	// While the engine is unavailable every call tries again...
	for range 2 {
		if _, err := connect(context.Background()); err == nil {
			t.Fatal("connect succeeded while the engine was down")
		}
	}

	// ...and once it works, the connection is kept and shared.
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			eng, err := connect(context.Background())
			if err != nil || eng != engine.Engine(fake) {
				t.Errorf("connect() = %v, %v; want the shared engine", eng, err)
			}
		}()
	}
	wg.Wait()

	if calls != 3 {
		t.Errorf("the engine was connected to %d times, want 2 failures and 1 success", calls)
	}
}
