// Package pipeline runs a list of named steps in order and undoes the
// completed ones when a later step fails.
//
// It knows nothing about deploys or Docker. A step is just a name and a few
// functions, which is what lets the same runner serve dry runs, step-by-step
// confirmation and history recording.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ErrAborted is returned when the user declines a step in confirm mode.
var ErrAborted = errors.New("aborted")

// Step is one unit of work.
//
// The function fields are closures: functions that capture variables from
// the place where they were created. That is how one step can hand a value
// (for example an allocated port) to a later step without the pipeline
// knowing anything about it.
type Step struct {
	// Name is shown to the user and recorded in the history.
	Name string

	// Manual returns the shell command a person would type to do this step
	// by hand. It is a function, not a string, because the command may
	// depend on values produced by earlier steps. Nil means there is no
	// meaningful manual equivalent.
	Manual func() string

	// Preview returns a description of what Run is about to change, for
	// example a diff. Unlike Manual it is shown in every mode, right before
	// the step runs. Nil or an empty result shows nothing.
	Preview func() string

	// Run performs the step. If it fails it must clean up after itself:
	// only steps that completed are undone.
	Run func(ctx context.Context) error

	// Undo reverses a completed Run. Nil means there is nothing to undo.
	Undo func(ctx context.Context) error
}

// Action says whether an event is about running or undoing a step.
type Action string

// The two kinds of events a pipeline produces.
const (
	ActionRun  Action = "run"
	ActionUndo Action = "undo"
)

// Event describes one executed action. Err is nil on success.
type Event struct {
	Step     string
	Action   Action
	Err      error
	Started  time.Time
	Duration time.Duration
}

// Options controls how a pipeline runs. The zero value runs every step
// without output.
type Options struct {
	// Out receives progress output. Nil discards it.
	Out io.Writer

	// DryRun prints the steps and their manual commands without running
	// anything.
	DryRun bool

	// Confirm, when set, is called before every step. Returning false
	// aborts the pipeline and undoes the steps completed so far.
	Confirm func(step Step) (bool, error)

	// OnEvent, when set, is called after every run and undo.
	OnEvent func(Event)

	// UndoTimeout limits how long the whole rollback may take. Zero means
	// one minute.
	UndoTimeout time.Duration
}

// Run executes steps in order. When a step fails, or the user aborts, the
// steps that had completed are undone in reverse order and the cause is
// returned.
func Run(ctx context.Context, steps []Step, opts Options) error {
	out := opts.Out
	if out == nil {
		out = io.Discard
	}

	// completed collects the steps that may need undoing.
	var completed []Step

	for i, step := range steps {
		fmt.Fprintf(out, "==> [%d/%d] %s\n", i+1, len(steps), step.Name)

		// The manual command is shown whenever the user is being asked to
		// read before acting: in a dry run and in confirm mode.
		if step.Manual != nil && (opts.DryRun || opts.Confirm != nil) {
			fmt.Fprintf(out, "    $ %s\n", step.Manual())
		}
		if step.Preview != nil {
			printIndented(out, step.Preview())
		}
		if opts.DryRun {
			continue
		}

		if opts.Confirm != nil {
			ok, err := opts.Confirm(step)
			if err != nil {
				return rollback(ctx, completed, opts, out, fmt.Errorf("confirm %q: %w", step.Name, err))
			}
			if !ok {
				return rollback(ctx, completed, opts, out, fmt.Errorf("%w before %q", ErrAborted, step.Name))
			}
		}

		started := time.Now()
		err := step.Run(ctx)
		emit(opts, Event{Step: step.Name, Action: ActionRun, Err: err, Started: started, Duration: time.Since(started)})

		if err != nil {
			return rollback(ctx, completed, opts, out, fmt.Errorf("%s: %w", step.Name, err))
		}
		completed = append(completed, step)
	}

	return nil
}

// rollback undoes completed steps, newest first, and returns cause combined
// with any errors the undo itself produced.
func rollback(ctx context.Context, completed []Step, opts Options, out io.Writer, cause error) error {
	timeout := opts.UndoTimeout
	if timeout == 0 {
		timeout = time.Minute
	}

	// The rollback must run even if ctx is the reason we are here: after
	// Ctrl+C the context is already cancelled, and using it would make
	// every undo fail immediately. WithoutCancel keeps the values of ctx
	// but drops its cancellation; the timeout puts a new limit in place.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	errs := []error{cause}

	// Reverse order: the last thing done is the first thing undone, the
	// same way nested operations are unwound.
	for i := len(completed) - 1; i >= 0; i-- {
		step := completed[i]
		if step.Undo == nil {
			continue
		}

		fmt.Fprintf(out, "<== Undo: %s\n", step.Name)

		started := time.Now()
		err := step.Undo(ctx)
		emit(opts, Event{Step: step.Name, Action: ActionUndo, Err: err, Started: started, Duration: time.Since(started)})

		if err != nil {
			// Keep going: one failed undo must not prevent the others.
			errs = append(errs, fmt.Errorf("undo %q: %w", step.Name, err))
		}
	}

	// With a single element Join returns an error that prints exactly like
	// cause, and errors.Is still finds everything inside.
	return errors.Join(errs...)
}

// printIndented writes text with every line indented to sit under a step
// heading.
func printIndented(out io.Writer, text string) {
	if text == "" {
		return
	}
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		fmt.Fprintf(out, "    %s\n", line)
	}
}

func emit(opts Options, ev Event) {
	if opts.OnEvent != nil {
		opts.OnEvent(ev)
	}
}
