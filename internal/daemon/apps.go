package daemon

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/onrcanogul/liman/internal/app"
	"github.com/onrcanogul/liman/internal/audit"
	"github.com/onrcanogul/liman/internal/store"
)

// An app deployed from CI is registered once, on the server: its name, the
// repository its images come from and its domain. Those are what its
// pipeline cannot change; everything else comes with every deploy, in the
// app.yaml the pipeline sends. Registering is the one step that needs a
// shell on the server, like creating a token: opening a door is done here,
// using it from anywhere.

func (d *Daemon) runApp(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return d.usageError("limand app create|list|delete")
	}
	switch args[0] {
	case "create":
		return d.appCreate(ctx, args[1:])
	case "list":
		return d.appList(ctx, args[1:])
	case "delete":
		return d.appDelete(ctx, args[1:])
	default:
		return d.usageError("limand app create|list|delete")
	}
}

func (d *Daemon) appCreate(ctx context.Context, args []string) int {
	const synopsis = "limand app create <name> --repo <repository> --domain <domain> [--key <name>] [--write-client <file>] [--url <url>]"

	fs := flag.NewFlagSet("app create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", "", "the image repository, without a tag: ghcr.io/team/orders-api")
	domain := fs.String("domain", "", "the app's domain: api.example.com")
	keyName := fs.String("key", "", "the deploy key's name (default <name>-ci)")
	writeClient := fs.String("write-client", "", "write the deploy key to this client.yaml instead of printing it")
	url := fs.String("url", "", "the trigger endpoint's URL to put into the client.yaml")

	if len(args) == 0 || len(args[0]) == 0 || args[0][0] == '-' {
		return d.usageError(synopsis)
	}
	name := args[0]
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *repo == "" || *domain == "" {
		return d.usageError(synopsis)
	}
	if *url != "" && *writeClient == "" {
		fmt.Fprintln(d.Stderr, "limand: --url only makes sense together with --write-client")
		return exitUsage
	}
	// Checked before anything is stored: a typo here would fix a wrong
	// value that no pipeline could correct.
	switch {
	case !app.ValidName(name):
		fmt.Fprintf(d.Stderr, "limand: %q is not a valid app name: 1-63 lowercase letters, digits or hyphens\n", name)
		return exitUsage
	case !app.ValidRepository(*repo):
		fmt.Fprintf(d.Stderr, "limand: --repo %q: give the image repository without a tag or digest, such as ghcr.io/team/%s; each deploy names its image by digest\n", *repo, name)
		return exitUsage
	case !app.ValidDomain(*domain):
		fmt.Fprintf(d.Stderr, "limand: --domain %q is not a valid domain, such as api.example.com\n", *domain)
		return exitUsage
	}
	if *keyName == "" {
		*keyName = name + "-ci"
	}

	st, err := d.openStore(ctx)
	if err != nil {
		return d.fail(err)
	}
	defer func() { _ = st.Close() }()

	if err := st.Register(ctx, store.Registration{Name: name, Repo: *repo, Domain: *domain}); err != nil {
		if errors.Is(err, store.ErrDomainTaken) {
			owner, _ := st.DomainOwner(ctx, *domain)
			return d.fail(fmt.Errorf("%s is registered to %s", *domain, owner))
		}
		return d.fail(err)
	}
	token, err := st.CreateScopedToken(ctx, *keyName, store.ScopeDeploy, time.Time{}, name)
	if err != nil {
		// A registration nobody can deploy to is no use: undo it.
		_, _ = st.Unregister(ctx, name)
		return d.fail(err)
	}
	rec := d.auditor(st)
	rec.Record(ctx, audit.ServerActor(), audit.AppRegister, name, *repo+", "+*domain, nil)
	rec.Record(ctx, audit.ServerActor(), audit.TokenCreate, "", *keyName+" ("+store.ScopeDeploy+", "+name+")", nil)

	code := d.handOver(ctx, st, newToken{
		name: *keyName, token: token, what: fmt.Sprintf("deploy key %q for %s", *keyName, name),
		writeClient: *writeClient, url: *url, scope: store.ScopeDeploy,
	})
	if code != exitOK {
		// Unregistering takes the keys with it.
		_, _ = st.Unregister(ctx, name)
		return code
	}
	fmt.Fprintf(d.Stdout, "Registered %s: images from %s, served at %s. Its pipeline deploys it with:\n  liman trigger --digest <digest> --file app.yaml\n", name, *repo, *domain)
	return exitOK
}

func (d *Daemon) appList(ctx context.Context, args []string) int {
	if len(args) != 0 {
		return d.usageError("limand app list")
	}
	st, err := d.openStore(ctx)
	if err != nil {
		return d.fail(err)
	}
	defer func() { _ = st.Close() }()

	regs, err := st.Registrations(ctx)
	if err != nil {
		return d.fail(err)
	}
	if len(regs) == 0 {
		fmt.Fprintln(d.Stdout, "No apps registered. Register one with: limand app create <name> --repo <repository> --domain <domain>")
		return exitOK
	}
	w := tabwriter.NewWriter(d.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tREPOSITORY\tDOMAIN\tREGISTERED\tLAST TRIGGERED")
	for _, r := range regs {
		last := "-"
		if state, found, err := st.DeployStateOf(ctx, r.Name); err == nil && found {
			last = state.Release + " at " + state.UpdatedAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Name, r.Repo, r.Domain, r.CreatedAt.Local().Format("2006-01-02 15:04"), last)
	}
	if err := w.Flush(); err != nil {
		return d.fail(err)
	}
	return exitOK
}

func (d *Daemon) appDelete(ctx context.Context, args []string) int {
	if len(args) != 1 {
		return d.usageError("limand app delete <name>")
	}
	name := args[0]
	st, err := d.openStore(ctx)
	if err != nil {
		return d.fail(err)
	}
	defer func() { _ = st.Close() }()

	changes, err := st.Unregister(ctx, name)
	if err != nil {
		return d.fail(err)
	}
	rec := d.auditor(st)
	rec.Record(ctx, audit.ServerActor(), audit.AppUnregister, name, "", nil)
	d.recordKeyChanges(ctx, rec, name, changes)
	fmt.Fprintf(d.Stdout, "Unregistered %s: its pipeline can no longer deploy it.\n", name)
	d.printKeyChanges(name, changes)
	fmt.Fprintf(d.Stdout, "What runs keeps running (liman stop %s).\n", name)
	return exitOK
}

// recordKeyChanges writes what happened to an app's keys into the audit
// trail, one record per key.
func (d *Daemon) recordKeyChanges(ctx context.Context, rec *audit.Recorder, app string, c store.KeyChanges) {
	for _, k := range c.Revoked {
		rec.Record(ctx, audit.ServerActor(), audit.TokenRevoke, "", k, nil)
	}
	for _, k := range c.Detached {
		rec.Record(ctx, audit.ServerActor(), audit.TokenRevoke, "", k+" (for "+app+" only; it keeps its other apps)", nil)
	}
	for _, k := range c.Expiring {
		rec.Record(ctx, audit.ServerActor(), audit.TokenRevoke, "", k+" (expires "+c.Until.UTC().Format(time.RFC3339)+")", nil)
	}
}

func (d *Daemon) printKeyChanges(app string, c store.KeyChanges) {
	if len(c.Revoked) > 0 {
		fmt.Fprintf(d.Stdout, "Revoked: %s.\n", strings.Join(c.Revoked, ", "))
	}
	if len(c.Expiring) > 0 {
		fmt.Fprintf(d.Stdout, "Still working until %s, then expiring: %s.\n", c.Until.Local().Format("2006-01-02 15:04"), strings.Join(c.Expiring, ", "))
	}
	if len(c.Detached) > 0 {
		fmt.Fprintf(d.Stdout, "No longer for %s, still for their other apps: %s.\n", app, strings.Join(c.Detached, ", "))
	}
}
