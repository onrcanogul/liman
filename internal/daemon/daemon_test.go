package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onrcanogul/liman/internal/api"
	"github.com/onrcanogul/liman/internal/config"
	"github.com/onrcanogul/liman/internal/engine"
	"github.com/onrcanogul/liman/internal/engine/enginetest"
	"github.com/onrcanogul/liman/internal/monitor"
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
		{"deploy key without app", []string{"token", "create", "ci", "--deploy"}, "needs --app"},
		{"deploy key read-only", []string{"token", "create", "ci", "--deploy", "--read-only", "--app", "web"}, "cannot be combined"},
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

func TestDaemonRestartsCrashedAppAndRecordsIt(t *testing.T) {
	td := newTestDaemon(t)
	writeConfig(t, td.Home, "daemon:\n  listen: 127.0.0.1:0\nmonitor:\n  interval: 1s\n")
	clientFile := filepath.Join(t.TempDir(), "client.yaml")
	td.Run(context.Background(), []string{"token", "create", "dev", "--write-client", clientFile})
	cfg, _ := config.LoadClient(clientFile)

	// An app that has crashed before the daemon even starts, as after a
	// reboot of the server.
	td.engine.Add(engine.Container{Name: "web-r1", App: "web", State: "exited", Release: "r1"})

	addr, stop, exit := startServing(t, td)
	client := &api.Client{BaseURL: "http://" + addr, Token: cfg.Token}

	// Nobody asks for it: the monitor must bring the app back by itself.
	deadline := time.Now().Add(10 * time.Second)
	for td.engine.Restarts("web-r1") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the daemon did not restart the crashed app")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The restart shows up in the history...
	var restart *api.Event
	for restart == nil {
		events, err := client.History(context.Background(), "web", 10)
		if err != nil {
			t.Fatal(err)
		}
		for i := range events {
			if events[i].Action == "restart" {
				restart = &events[i]
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the restart was not recorded in the history")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !restart.OK || restart.Release != "r1" || restart.Step != "Restart container web-r1: container is exited (exit code 0)" {
		t.Errorf("history entry = %+v", *restart)
	}

	// ...and in the listing.
	apps, err := client.Apps(context.Background())
	if err != nil || len(apps) != 1 {
		t.Fatalf("Apps() = %v, %v", apps, err)
	}
	if apps[0].Health == "" || apps[0].Restarts != 1 {
		t.Errorf("listing = %+v, want a health verdict and one restart", apps[0])
	}

	stop()
	if code := <-exit; code != exitOK {
		t.Errorf("exit code = %d, stderr: %s", code, td.stderr.String())
	}
	for _, want := range []string{"health monitor started", "restarting app", "health monitor stopped"} {
		if !strings.Contains(td.stderr.String(), want) {
			t.Errorf("log does not contain %q:\n%s", want, td.stderr.String())
		}
	}
}

func TestMonitorCanBeDisabled(t *testing.T) {
	td := newTestDaemon(t)
	writeConfig(t, td.Home, "daemon:\n  listen: 127.0.0.1:0\nmonitor:\n  disabled: true\n  interval: 1s\n")
	td.engine.Add(engine.Container{Name: "web-r1", App: "web", State: "exited"})

	_, stop, exit := startServing(t, td)
	time.Sleep(1500 * time.Millisecond) // longer than the interval
	stop()
	<-exit

	if got := td.engine.Restarts("web-r1"); got != 0 {
		t.Errorf("the app was restarted %d times although the monitor is disabled", got)
	}
	if strings.Contains(td.stderr.String(), "health monitor started") {
		t.Error("the monitor started although it is disabled")
	}
}

func TestMasterKeyIsCreatedOnceAndReused(t *testing.T) {
	td := newTestDaemon(t)
	keyFile := filepath.Join(td.Home, "master.key")
	clientFile := filepath.Join(t.TempDir(), "client.yaml")
	td.Run(context.Background(), []string{"token", "create", "dev", "--write-client", clientFile})
	cfg, _ := config.LoadClient(clientFile)

	// First start: the key is created, private, and announced.
	addr, stop, exit := startServing(t, td)
	client := &api.Client{BaseURL: "http://" + addr, Token: cfg.Token}
	if err := client.SetSecret(context.Background(), "web", "API_KEY", []byte("k-123")); err != nil {
		t.Fatalf("SetSecret() error = %v", err)
	}
	stop()
	<-exit

	info, err := os.Stat(keyFile)
	if err != nil {
		t.Fatalf("no master key was created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("master key mode = %o, want 600", perm)
	}
	if !strings.Contains(td.stderr.String(), "created a new master key") {
		t.Errorf("the log does not announce the new key:\n%s", td.stderr.String())
	}
	before, _ := os.ReadFile(keyFile)

	// Second start: the same key is used, so the stored secret still works.
	td.engine.Add(engine.Container{Name: "placeholder", App: "placeholder"})
	addr, stop, exit = startServing(t, td)
	client.BaseURL = "http://" + addr
	const definition = "name: web\nimage: web:1.0\ndomain: web.example.com\nport: 80\nsecrets:\n  - API_KEY\n"
	if _, err := client.Deploy(context.Background(), []byte(definition), api.DeployOptions{}, io.Discard); err != nil {
		t.Errorf("Deploy() with a secret stored before the restart error = %v", err)
	}
	stop()
	<-exit

	if after, _ := os.ReadFile(keyFile); string(after) != string(before) {
		t.Error("the master key changed between starts")
	}
	// The key itself must never be logged.
	if strings.Contains(td.stderr.String(), strings.TrimSpace(string(before))) {
		t.Error("the log contains the master key")
	}
}

func TestDaemonRefusesToStartWhenKeyIsLostButSecretsExist(t *testing.T) {
	td := newTestDaemon(t)
	keyFile := filepath.Join(td.Home, "master.key")
	clientFile := filepath.Join(t.TempDir(), "client.yaml")
	td.Run(context.Background(), []string{"token", "create", "dev", "--write-client", clientFile})
	cfg, _ := config.LoadClient(clientFile)

	addr, stop, exit := startServing(t, td)
	client := &api.Client{BaseURL: "http://" + addr, Token: cfg.Token}
	if err := client.SetSecret(context.Background(), "web", "API_KEY", []byte("k-123")); err != nil {
		t.Fatal(err)
	}
	stop()
	<-exit

	// The key file is lost (a restore that only brought back the database).
	if err := os.Remove(keyFile); err != nil {
		t.Fatal(err)
	}

	code := td.Run(context.Background(), nil)

	if code != exitError {
		t.Fatalf("exit code = %d, want the daemon to refuse to start", code)
	}
	for _, want := range []string{"master key", "is missing", "1 secret is stored", "restore the key file"} {
		if !strings.Contains(td.stderr.String(), want) {
			t.Errorf("stderr does not contain %q:\n%s", want, td.stderr.String())
		}
	}
	// Above all, it must not have quietly made a new key.
	if _, err := os.Stat(keyFile); err == nil {
		t.Error("a new master key was created although secrets encrypted with the old one exist")
	}
}

func TestDaemonRefusesAKeyReadableByOthers(t *testing.T) {
	td := newTestDaemon(t)
	keyFile := filepath.Join(td.Home, "master.key")
	// 32 zero bytes in base64: a well-formed key with the wrong permissions.
	if err := os.WriteFile(keyFile, []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyFile, 0o644); err != nil {
		t.Fatal(err)
	}

	code := td.Run(context.Background(), nil)

	if code != exitError || !strings.Contains(td.stderr.String(), "readable by others") {
		t.Errorf("code %d, stderr %q; want a refusal over the key's permissions", code, td.stderr.String())
	}
}

func TestHealthChecksAreRecordedAndOldOnesRemoved(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "liman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	const retention = 24 * time.Hour
	record := recordChecks(st, slog.New(slog.DiscardHandler), retention)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	count := func() int {
		t.Helper()
		buckets, err := st.HealthBuckets(ctx, "web", now.Add(-10*24*time.Hour), now.Add(time.Hour), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		total := 0
		for _, b := range buckets {
			total += b.OK + b.Failed
		}
		return total
	}

	// A check from two days ago, as left behind by an earlier run.
	record(monitor.Check{App: "web", Time: now.Add(-48 * time.Hour), OK: true})
	if got := count(); got != 1 {
		t.Fatalf("samples after the first check = %d, want 1", got)
	}

	// The next check is outside the pruning interval, so the old one goes.
	record(monitor.Check{App: "web", Time: now, OK: false, Probed: true, Duration: 20 * time.Millisecond})
	if got := count(); got != 1 {
		t.Errorf("samples after pruning = %d, want only the new one", got)
	}

	// Checks that follow closely do not prune again, and are all kept.
	for i := 1; i <= 3; i++ {
		record(monitor.Check{App: "web", Time: now.Add(time.Duration(i) * time.Second), OK: true})
	}
	if got := count(); got != 4 {
		t.Errorf("samples after three more checks = %d, want 4", got)
	}
}

func TestDaemonServesThePanelUnlessDisabled(t *testing.T) {
	status := func(t *testing.T, config string) int {
		t.Helper()
		td := newTestDaemon(t)
		writeConfig(t, td.Home, config)
		addr, stop, exit := startServing(t, td)
		defer func() { stop(); <-exit }()

		resp, err := http.Get("http://" + addr + "/ui/")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if got := status(t, "daemon:\n  listen: 127.0.0.1:0\n"); got != http.StatusOK {
		t.Errorf("GET /ui/ = %d, want 200", got)
	}
	// Disabled, the path is like any other unknown one: it needs a token.
	if got := status(t, "daemon:\n  listen: 127.0.0.1:0\npanel:\n  disabled: true\n"); got != http.StatusUnauthorized {
		t.Errorf("GET /ui/ with the panel disabled = %d, want 401", got)
	}
}

func TestTokenCreateForApps(t *testing.T) {
	td := newTestDaemon(t)
	ctx := context.Background()

	if code := td.Run(ctx, []string{"token", "create", "ci", "--app", "orders-api", "--app", "billing"}); code != exitOK {
		t.Fatalf("token create --app: code %d, stderr %q", code, td.stderr.String())
	}
	if !strings.Contains(td.stdout.String(), `token "ci" for orders-api, billing`) {
		t.Errorf("token create --app says: %q", td.stdout.String())
	}
	token := tokenRe.FindString(td.stdout.String())

	st, err := store.Open(ctx, filepath.Join(td.Home, "liman.db"))
	if err != nil {
		t.Fatal(err)
	}
	info, ok, err := st.LookupToken(ctx, token)
	_ = st.Close()
	if err != nil || !ok || !slices.Equal(info.Apps, []string{"billing", "orders-api"}) || info.Scope != store.ScopeAdmin {
		t.Errorf("the created token = %+v, %v, %v; want admin, limited to billing and orders-api", info, ok, err)
	}

	if code := td.Run(ctx, []string{"token", "create", "laptop"}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	if code := td.Run(ctx, []string{"token", "list"}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	out := td.stdout.String()
	if !regexp.MustCompile(`(?m)^ci\s+admin\s+billing,orders-api\s`).MatchString(out) || !regexp.MustCompile(`(?m)^laptop\s+admin\s+all\s`).MatchString(out) {
		t.Errorf("token list does not show the apps:\n%s", out)
	}

	// A name that is not an app name is a typo, refused before anything
	// is stored.
	for _, bad := range []string{"Orders", "a b", ""} {
		if code := td.Run(ctx, []string{"token", "create", "typo", "--app", bad}); code != exitUsage {
			t.Errorf("token create --app %q: code %d, want %d", bad, code, exitUsage)
		}
	}
	if code := td.Run(ctx, []string{"token", "list"}); code != exitOK || strings.Contains(td.stdout.String(), "typo") {
		t.Errorf("a refused token was stored:\n%s", td.stdout.String())
	}
}

func TestTokenCreateReadOnly(t *testing.T) {
	td := newTestDaemon(t)
	ctx := context.Background()

	if code := td.Run(ctx, []string{"token", "create", "wallboard", "--read-only"}); code != exitOK {
		t.Fatalf("token create --read-only: code %d, stderr %q", code, td.stderr.String())
	}
	token := tokenRe.FindString(td.stdout.String())

	st, err := store.Open(ctx, filepath.Join(td.Home, "liman.db"))
	if err != nil {
		t.Fatal(err)
	}
	info, ok, err := st.LookupToken(ctx, token)
	_ = st.Close()
	if err != nil || !ok || info.Scope != store.ScopeRead {
		t.Errorf("the created token = %+v, %v, %v; want scope read", info, ok, err)
	}

	if code := td.Run(ctx, []string{"token", "create", "laptop"}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	if code := td.Run(ctx, []string{"token", "list"}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	out := td.stdout.String()
	if !regexp.MustCompile(`wallboard\s+read`).MatchString(out) || !regexp.MustCompile(`laptop\s+admin`).MatchString(out) {
		t.Errorf("token list does not show the scopes:\n%s", out)
	}

	// A name that could forge a log field or move the terminal's cursor.
	if code := td.Run(ctx, []string{"token", "create", "a b"}); code != exitError {
		t.Errorf("token create with a space in the name: code %d, want %d", code, exitError)
	}
}

func TestRegisteredAppIsDeployedFromItsPipelineOnly(t *testing.T) {
	td := newTestDaemon(t)
	writeConfig(t, td.Home, "daemon:\n  listen: 127.0.0.1:0\nhooks:\n  listen: 127.0.0.1:0\n")
	ctx := context.Background()

	adminFile := filepath.Join(t.TempDir(), "admin.yaml")
	keyFile := filepath.Join(t.TempDir(), "ci.yaml")
	if code := td.Run(ctx, []string{"token", "create", "dev", "--write-client", adminFile}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	if code := td.Run(ctx, []string{"app", "create", "web", "--repo", "registry.example.com/web", "--domain", "web.example.com", "--write-client", keyFile}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	if out := td.stdout.String(); !strings.Contains(out, `deploy key "web-ci" for web`) || !strings.Contains(out, "Registered web") {
		t.Errorf("app create says: %q", out)
	}
	if code := td.Run(ctx, []string{"token", "list"}); code != exitOK || !regexp.MustCompile(`(?m)^web-ci\s+deploy\s+web\s`).MatchString(td.stdout.String()) {
		t.Errorf("token list does not show the deploy key:\n%s", td.stdout.String())
	}
	admin, _ := config.LoadClient(adminFile)
	key, err := config.LoadClient(keyFile)
	if err != nil {
		t.Fatal(err)
	}

	hooks := make(chan string, 1)
	td.HooksReady = func(a string) { hooks <- a }
	addr, stop, exit := startServing(t, td)
	hooksAddr := <-hooks

	d := "sha256:" + strings.Repeat("ab", 32)
	td.engine.Registry = map[string]bool{"registry.example.com/web@" + d: true}
	ci := &api.Client{BaseURL: "http://" + hooksAddr, Token: key.Token}
	req := api.TriggerRequest{
		App: "web", Digest: d,
		Definition: "name: web\nimage: registry.example.com/web\ndomain: web.example.com\nport: 80\nenv:\n  TOKEN: ${TOKEN}\n",
		Vars:       map[string]string{"TOKEN": "t0p"},
	}
	if res, err := ci.Trigger(ctx, req, io.Discard); err != nil || !res.OK {
		t.Errorf("Trigger() = %+v, %v", res, err)
	}
	if res, err := ci.Trigger(ctx, req, io.Discard); err != nil || !res.Unchanged {
		t.Errorf("the same Trigger() again = %+v, %v; want unchanged", res, err)
	}
	// The key does nothing on the API, and the API's tokens nothing on
	// the hooks listener.
	if _, err := (&api.Client{BaseURL: "http://" + addr, Token: key.Token}).Apps(ctx); err == nil {
		t.Error("the deploy key listed the apps")
	}
	if _, err := (&api.Client{BaseURL: "http://" + hooksAddr, Token: admin.Token}).Trigger(ctx, req, io.Discard); err == nil {
		t.Error("an admin token triggered a deploy")
	}

	stop()
	if code := <-exit; code != exitOK {
		t.Errorf("exit code = %d: %s", code, td.stderr.String())
	}
	for _, secret := range []string{key.Token, "t0p"} {
		if strings.Contains(td.stderr.String(), secret) {
			t.Errorf("the log contains %q", secret)
		}
	}

	if code := td.Run(ctx, []string{"app", "list"}); code != exitOK || !regexp.MustCompile(`(?m)^web\s+registry.example.com/web\s+web.example.com\s.*\s\S+ at `).MatchString(td.stdout.String()) {
		t.Errorf("app list:\n%s", td.stdout.String())
	}
	if code := td.Run(ctx, []string{"app", "delete", "web"}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	if code := td.Run(ctx, []string{"app", "list"}); code != exitOK || !strings.Contains(td.stdout.String(), "No apps registered") {
		t.Errorf("app list after delete:\n%s", td.stdout.String())
	}
}

func TestAppCreateRefusals(t *testing.T) {
	td := newTestDaemon(t)
	ctx := context.Background()
	for _, tt := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"app", "create", "web", "--repo", "r.example.com/web"}, exitUsage, "usage: limand app create"},
		{[]string{"app", "create", "Web", "--repo", "r.example.com/web", "--domain", "w.example.com"}, exitUsage, "not a valid app name"},
		{[]string{"app", "create", "web", "--repo", "r.example.com/web:1.0", "--domain", "w.example.com"}, exitUsage, "without a tag"},
		{[]string{"app", "create", "web", "--repo", "r.example.com/web", "--domain", "localhost"}, exitUsage, "not a valid domain"},
		// Without hooks.listen there is no address to write: nothing is
		// left behind.
		{[]string{"app", "create", "web", "--repo", "r.example.com/web", "--domain", "w.example.com", "--write-client", filepath.Join(t.TempDir(), "c.yaml")}, exitError, "hooks.listen is not set"},
	} {
		td.stderr.buf.Reset()
		if code := td.Run(ctx, tt.args); code != tt.code || !strings.Contains(td.stderr.String(), tt.want) {
			t.Errorf("%v: code %d, stderr %q; want %d and %q", tt.args, code, td.stderr.String(), tt.code, tt.want)
		}
	}
	if code := td.Run(ctx, []string{"app", "list"}); code != exitOK || !strings.Contains(td.stdout.String(), "No apps registered") {
		t.Errorf("a refused app was kept:\n%s", td.stdout.String())
	}
	if code := td.Run(ctx, []string{"token", "list"}); code != exitOK || strings.Contains(td.stdout.String(), "web-ci") {
		t.Errorf("a refused app's key was kept:\n%s", td.stdout.String())
	}

	// A deploy key needs a registered app; a domain belongs to one app.
	if code := td.Run(ctx, []string{"token", "create", "ci", "--deploy", "--app", "web"}); code != exitError || !strings.Contains(td.stderr.String(), "not registered") {
		t.Errorf("deploy key for an app that is not registered: code %d, stderr %q", code, td.stderr.String())
	}
	if code := td.Run(ctx, []string{"app", "create", "web", "--repo", "r.example.com/web", "--domain", "w.example.com"}); code != exitOK {
		t.Fatal(td.stderr.String())
	}
	if code := td.Run(ctx, []string{"app", "create", "api", "--repo", "r.example.com/api", "--domain", "w.example.com"}); code != exitError || !strings.Contains(td.stderr.String(), "registered to web") {
		t.Errorf("a taken domain: code %d, stderr %q", code, td.stderr.String())
	}
}
