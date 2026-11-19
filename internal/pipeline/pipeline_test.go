package pipeline

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// recorder builds steps that append to a shared log, so tests can assert on
// the exact order of runs and undos.
type recorder struct {
	log []string
}

// step returns a step named name. If runErr is not nil, Run fails with it.
func (r *recorder) step(name string, runErr error) Step {
	return Step{
		Name:   name,
		Manual: func() string { return "do " + name },
		Run: func(context.Context) error {
			r.log = append(r.log, "run "+name)
			return runErr
		},
		Undo: func(context.Context) error {
			r.log = append(r.log, "undo "+name)
			return nil
		},
	}
}

func TestRunAllStepsInOrder(t *testing.T) {
	var r recorder
	steps := []Step{r.step("a", nil), r.step("b", nil), r.step("c", nil)}

	if err := Run(context.Background(), steps, Options{}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	want := []string{"run a", "run b", "run c"}
	if !slices.Equal(r.log, want) {
		t.Errorf("log = %v, want %v", r.log, want)
	}
}

func TestRunUndoesCompletedStepsInReverse(t *testing.T) {
	var r recorder
	boom := errors.New("boom")
	steps := []Step{r.step("a", nil), r.step("b", nil), r.step("c", boom), r.step("d", nil)}

	err := Run(context.Background(), steps, Options{})

	if !errors.Is(err, boom) {
		t.Fatalf("Run() error = %v, want it to wrap boom", err)
	}
	if !strings.Contains(err.Error(), "c: boom") {
		t.Errorf("error %q does not name the failed step", err)
	}

	// c failed, so it is not undone (it cleans up after itself); d never
	// ran; b and a are undone newest first.
	want := []string{"run a", "run b", "run c", "undo b", "undo a"}
	if !slices.Equal(r.log, want) {
		t.Errorf("log = %v, want %v", r.log, want)
	}
}

func TestRunSkipsStepsWithoutUndo(t *testing.T) {
	var r recorder
	noUndo := r.step("a", nil)
	noUndo.Undo = nil
	steps := []Step{noUndo, r.step("b", nil), r.step("c", errors.New("boom"))}

	_ = Run(context.Background(), steps, Options{})

	want := []string{"run a", "run b", "run c", "undo b"}
	if !slices.Equal(r.log, want) {
		t.Errorf("log = %v, want %v", r.log, want)
	}
}

func TestRunReportsUndoFailures(t *testing.T) {
	var r recorder
	undoErr := errors.New("cannot undo")
	failing := r.step("a", nil)
	failing.Undo = func(context.Context) error { return undoErr }
	boom := errors.New("boom")
	steps := []Step{failing, r.step("b", nil), r.step("c", boom)}

	err := Run(context.Background(), steps, Options{})

	// Both the original failure and the undo failure must be reachable.
	if !errors.Is(err, boom) || !errors.Is(err, undoErr) {
		t.Errorf("Run() error = %v, want it to contain both errors", err)
	}
	// A failing undo must not stop the remaining undos.
	if !slices.Contains(r.log, "undo b") {
		t.Errorf("log = %v, want b to be undone", r.log)
	}
}

func TestRunUndoesEvenWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	undoSawLiveContext := false
	steps := []Step{
		{
			Name: "a",
			Run:  func(context.Context) error { return nil },
			Undo: func(ctx context.Context) error {
				undoSawLiveContext = ctx.Err() == nil
				return nil
			},
		},
		{
			Name: "b",
			// Simulates Ctrl+C arriving while this step runs.
			Run: func(ctx context.Context) error {
				cancel()
				return ctx.Err()
			},
		},
	}

	err := Run(ctx, steps, Options{})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if !undoSawLiveContext {
		t.Error("undo received a cancelled context and could not do its work")
	}
}

func TestDryRunRunsNothing(t *testing.T) {
	var r recorder
	var out bytes.Buffer
	steps := []Step{r.step("a", nil), r.step("b", nil)}

	if err := Run(context.Background(), steps, Options{Out: &out, DryRun: true}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(r.log) != 0 {
		t.Errorf("log = %v, want nothing to run", r.log)
	}
	want := "==> [1/2] a\n    $ do a\n==> [2/2] b\n    $ do b\n"
	if out.String() != want {
		t.Errorf("output =\n%s\nwant\n%s", out.String(), want)
	}
}

func TestConfirmDeclineAbortsAndUndoes(t *testing.T) {
	var r recorder
	steps := []Step{r.step("a", nil), r.step("b", nil), r.step("c", nil)}

	// Approve the first step, decline the second.
	asked := 0
	confirm := func(Step) (bool, error) {
		asked++
		return asked == 1, nil
	}

	err := Run(context.Background(), steps, Options{Confirm: confirm})

	if !errors.Is(err, ErrAborted) {
		t.Fatalf("Run() error = %v, want ErrAborted", err)
	}
	want := []string{"run a", "undo a"}
	if !slices.Equal(r.log, want) {
		t.Errorf("log = %v, want %v", r.log, want)
	}
}

func TestOnEventReceivesRunsAndUndos(t *testing.T) {
	var r recorder
	boom := errors.New("boom")
	steps := []Step{r.step("a", nil), r.step("b", boom)}

	var got []string
	onEvent := func(ev Event) {
		result := "ok"
		if ev.Err != nil {
			result = "failed"
		}
		got = append(got, string(ev.Action)+" "+ev.Step+" "+result)
	}

	_ = Run(context.Background(), steps, Options{OnEvent: onEvent})

	want := []string{"run a ok", "run b failed", "undo a ok"}
	if !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

func TestPreviewIsShownInEveryMode(t *testing.T) {
	newSteps := func(ran *bool) []Step {
		return []Step{{
			Name:    "write file",
			Preview: func() string { return "- old\n+ new\n" },
			Run:     func(context.Context) error { *ran = true; return nil },
		}}
	}

	want := "==> [1/1] write file\n    - old\n    + new\n"

	for _, dryRun := range []bool{false, true} {
		var out bytes.Buffer
		ran := false

		if err := Run(context.Background(), newSteps(&ran), Options{Out: &out, DryRun: dryRun}); err != nil {
			t.Fatalf("Run() error = %v", err)
		}

		if out.String() != want {
			t.Errorf("dryRun=%v output = %q, want %q", dryRun, out.String(), want)
		}
		if ran == dryRun {
			t.Errorf("dryRun=%v but ran=%v", dryRun, ran)
		}
	}
}
