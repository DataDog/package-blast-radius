package dataset

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// newTestConfirmer builds a confirmer driven by a string, forcing interactive
// mode because a strings.Reader is never a terminal.
func newTestConfirmer(input string, autoApprove bool) (*confirmer, *bytes.Buffer) {
	out := &bytes.Buffer{}
	c := newConfirmer(strings.NewReader(input), out, autoApprove)
	c.interactive = true
	return c, out
}

func TestConfirmAcceptsYes(t *testing.T) {
	for _, input := range []string{"y\n", "Y\n", "yes\n", "YES\n", "  y  \n"} {
		c, _ := newTestConfirmer(input, false)
		if err := c.confirm("Proceed?"); err != nil {
			t.Errorf("confirm(%q) = %v, want nil", input, err)
		}
	}
}

func TestConfirmRejectsEverythingElse(t *testing.T) {
	// An empty line is the default, and must not be read as consent.
	for _, input := range []string{"n\n", "no\n", "\n", "maybe\n", "yep\n", ""} {
		c, _ := newTestConfirmer(input, false)
		err := c.confirm("Proceed?")
		if !errors.Is(err, ErrAborted) {
			t.Errorf("confirm(%q) = %v, want ErrAborted", input, err)
		}
	}
}

// A run asks several times (bucket, deletion, cost), and buffering stdin afresh
// each time would swallow every answer after the first.
func TestConfirmReadsSuccessiveAnswers(t *testing.T) {
	c, _ := newTestConfirmer("y\nn\ny\n", false)

	for i, want := range []bool{true, false, true} {
		err := c.confirm("Proceed?")
		if got := err == nil; got != want {
			t.Errorf("answer %d: accepted = %v, want %v (err %v)", i, got, want, err)
		}
	}
}

func TestConfirmAutoApproveDoesNotReadStdin(t *testing.T) {
	// "n" would be a refusal if it were read, which proves --yes short-circuits.
	c, out := newTestConfirmer("n\n", true)
	if err := c.confirm("Proceed?"); err != nil {
		t.Fatalf("confirm with autoApprove = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "auto-confirmed") {
		t.Errorf("output %q should say it was auto-confirmed", out.String())
	}
}

func TestConfirmRefusesWhenNotInteractive(t *testing.T) {
	out := &bytes.Buffer{}
	// No forceInteractive: a strings.Reader is not a terminal.
	c := newConfirmer(strings.NewReader("y\n"), out, false)

	err := c.confirm("Proceed?")
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("confirm = %v, want ErrAborted", err)
	}
	// The message has to say how to proceed, or a CI failure is a dead end.
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("error %q should mention --yes", err)
	}
}

func TestConfirmPromptIsShown(t *testing.T) {
	c, out := newTestConfirmer("y\n", false)
	c.confirm("Delete 900 objects?")
	if !strings.Contains(out.String(), "Delete 900 objects?") {
		t.Errorf("output %q should contain the prompt", out.String())
	}
	if !strings.Contains(out.String(), "[y/N]") {
		t.Errorf("output %q should show that no is the default", out.String())
	}
}
