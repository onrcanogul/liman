package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/onrcanogul/liman/internal/api"
	"github.com/onrcanogul/liman/internal/app"
	"github.com/onrcanogul/liman/internal/engine"
)

// readDefinition reads app.yaml for a pipeline command, and checks its
// app's name against want, when given.
func readDefinition(file, want string) (*app.App, []byte, error) {
	definition, err := os.ReadFile(file)
	if err != nil {
		return nil, nil, fmt.Errorf("read app definition: %w", err)
	}
	a, err := app.Decode(definition)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", file, err)
	}
	if want != "" && want != a.Name {
		return nil, nil, fmt.Errorf("%s is the definition of %q, not %q", file, a.Name, want)
	}
	if !app.ValidName(a.Name) {
		return nil, nil, fmt.Errorf("%s: invalid app name %q", file, a.Name)
	}
	return a, definition, nil
}

func (c *CLI) runPreview(ctx context.Context, args []string) int {
	const synopsis = "liman preview --file <app.yaml> [--digest <sha256:...>] [--app <app>] [--markdown]"

	fs := flag.NewFlagSet("preview", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("file", "", "the app's definition, app.yaml")
	digest := fs.String("digest", "", "the digest of the image, to check that the registry has it")
	name := fs.String("app", "", "the app; checked against the definition's name")
	markdown := fs.Bool("markdown", false, "write a Markdown summary, for a pull request comment")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *file == "" {
		return c.usageError(synopsis)
	}
	if *digest != "" && !engine.ValidDigest(*digest) {
		return c.fail(fmt.Errorf("invalid digest %q: want sha256: and 64 hexadecimal digits", *digest))
	}
	a, definition, err := readDefinition(*file, *name)
	if err != nil {
		return c.fail(err)
	}

	// No variable values: a pull request's pipeline should not have the
	// secrets, and the daemon compares the file as written.
	p, err := c.API.Preview(ctx, api.TriggerRequest{App: a.Name, Digest: *digest, Definition: string(definition)})
	if err != nil {
		return c.fail(err)
	}
	if *markdown {
		fmt.Fprint(c.Stdout, previewMarkdown(p))
	} else {
		fmt.Fprint(c.Stdout, previewText(p))
	}
	if !p.OK {
		return exitError
	}
	return exitOK
}

const notCompared = "values are compared as written in app.yaml; whether a secret's value changed is not known to a preview"

// previewText is the preview for a terminal or a CI log.
func previewText(p *api.Preview) string {
	var b strings.Builder
	if p.OK {
		fmt.Fprintf(&b, "%s: a deploy of this definition would be accepted\n", p.App)
	} else {
		fmt.Fprintf(&b, "%s: a deploy of this definition would be refused\n", p.App)
	}
	switch {
	case !p.Compared && !p.OK:
		// Refused before it could be compared: the problems say why.
	case !p.Compared && p.Running == "":
		b.WriteString("  nothing to compare with: no triggered deploy runs, so the next one deploys all of it\n")
	case !p.Compared:
		fmt.Fprintf(&b, "  nothing to compare with: release %s runs, but was deployed before previews kept what to compare\n", p.Running)
	case len(p.Changed)+len(p.Added)+len(p.Removed) == 0:
		fmt.Fprintf(&b, "  app.yaml: no change from release %s\n", p.Running)
	default:
		fmt.Fprintf(&b, "  compared with release %s:\n", p.Running)
		for _, l := range []struct {
			label string
			paths []string
		}{{"changed", p.Changed}, {"added", p.Added}, {"removed", p.Removed}} {
			if len(l.paths) > 0 {
				fmt.Fprintf(&b, "    %-8s %s\n", l.label+":", strings.Join(l.paths, ", "))
			}
		}
	}
	if p.Compared {
		fmt.Fprintf(&b, "  (%s)\n", notCompared)
	}
	if im := p.Image; im != nil {
		switch {
		case !im.Exists:
			fmt.Fprintf(&b, "  image %s: not in the registry\n", im.Digest)
		case im.Running:
			fmt.Fprintf(&b, "  image %s: in the registry, and already running\n", im.Digest)
		default:
			fmt.Fprintf(&b, "  image %s: in the registry\n", im.Digest)
		}
	}
	if len(p.Variables) > 0 {
		fmt.Fprintf(&b, "  the deploy's pipeline must set: %s\n", strings.Join(p.Variables, ", "))
	}
	if len(p.Problems) > 0 {
		b.WriteString("problems:\n")
		for _, pr := range p.Problems {
			fmt.Fprintf(&b, "  - %s\n", pr)
		}
	}
	return b.String()
}

// previewMarkdown is the preview for a pull request comment or a CI
// summary. Whoever opens the pull request writes app.yaml, and with it
// the paths and the problems shown here: everything from it is put in
// code spans or escaped, so that it cannot add links, images or mentions
// to the comment.
func previewMarkdown(p *api.Preview) string {
	var b strings.Builder
	verdict := "✅ a deploy of this definition would be accepted"
	if !p.OK {
		verdict = "❌ a deploy of this definition would be refused"
	}
	fmt.Fprintf(&b, "### liman preview: %s\n\n%s\n\n", mdCode(p.App), verdict)

	b.WriteString("| | |\n|---|---|\n")
	switch {
	case !p.Compared && !p.OK:
		// Refused before it could be compared: the problems say why.
		b.WriteString("| Compared with | nothing: see the problems |\n")
	case !p.Compared && p.Running == "":
		b.WriteString("| Compared with | nothing yet: the next deploy deploys all of it |\n")
	case !p.Compared:
		fmt.Fprintf(&b, "| Compared with | nothing: release %s predates previews |\n", mdCode(p.Running))
	default:
		fmt.Fprintf(&b, "| Compared with | release %s |\n", mdCode(p.Running))
		if len(p.Changed)+len(p.Added)+len(p.Removed) == 0 {
			b.WriteString("| app.yaml | no change |\n")
		}
		for _, l := range []struct {
			label string
			paths []string
		}{{"Changed", p.Changed}, {"Added", p.Added}, {"Removed", p.Removed}} {
			if len(l.paths) > 0 {
				fmt.Fprintf(&b, "| %s | %s |\n", l.label, mdCodes(l.paths))
			}
		}
	}
	if im := p.Image; im != nil {
		state := "in the registry"
		switch {
		case !im.Exists:
			state = "**not in the registry**"
		case im.Running:
			state = "in the registry, already running"
		}
		fmt.Fprintf(&b, "| Image | %s %s |\n", mdCode(im.Digest), state)
	}
	if len(p.Variables) > 0 {
		fmt.Fprintf(&b, "| Variables the deploy needs | %s |\n", mdCodes(p.Variables))
	}
	if len(p.Problems) > 0 {
		b.WriteString("\n**Problems**\n\n")
		for _, pr := range p.Problems {
			fmt.Fprintf(&b, "- %s\n", mdCode(pr))
		}
	}
	if p.Compared {
		fmt.Fprintf(&b, "\n_%s._\n", strings.ToUpper(notCompared[:1])+notCompared[1:])
	}
	return b.String()
}

// mdCode puts s in a code span, in which Markdown makes nothing of the
// text. A code span ends at a backtick and a table cell at a "|", and a
// line break would end the list item or the table: those are replaced.
func mdCode(s string) string {
	r := strings.NewReplacer("`", "'", "|", "¦", "\n", " ", "\r", " ")
	return "`" + r.Replace(s) + "`"
}

func mdCodes(list []string) string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = mdCode(s)
	}
	return strings.Join(out, ", ")
}
