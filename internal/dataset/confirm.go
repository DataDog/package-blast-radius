package dataset

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ErrAborted is returned when the user declines a confirmation prompt. It is a
// clean refusal rather than a failure, so callers report it without a stack of
// wrapping context.
var ErrAborted = errors.New("aborted")

// confirmer gates every irreversible or billable action behind an interactive
// yes. Nothing that costs money or mutates cloud state happens without one.
type confirmer struct {
	// in is buffered once and reused — a fresh bufio.Reader per prompt discards
	// what the previous one read ahead, losing every answer after the first when
	// stdin is a pipe.
	in          *bufio.Reader
	out         io.Writer
	autoApprove bool
	// interactive is false when stdin isn't a terminal; we refuse rather than
	// block forever on a read nobody can answer.
	interactive bool
}

func newConfirmer(in io.Reader, out io.Writer, autoApprove bool) *confirmer {
	return &confirmer{
		in:          bufio.NewReader(in),
		out:         out,
		autoApprove: autoApprove,
		interactive: isTerminal(in),
	}
}

// isTerminal reports whether r is an interactive terminal. Anything that is not
// an *os.File (a pipe, a bytes.Buffer in tests) is not.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// confirm prints prompt and waits for a yes. It returns ErrAborted on anything
// else, so a caller can simply propagate the error to stop the run.
func (c *confirmer) confirm(prompt string) error {
	if c.autoApprove {
		fmt.Fprintf(c.out, "%s%s [auto-confirmed with --yes]\n", reportIndent, prompt)
		return nil
	}
	if !c.interactive {
		return fmt.Errorf("%s\nnot running interactively; re-run with --yes to confirm: %w", prompt, ErrAborted)
	}

	fmt.Fprintf(c.out, "%s%s [y/N] ", reportIndent, prompt)

	answer, err := c.in.ReadString('\n')
	// A closed stdin (EOF) is a refusal, not a yes.
	if err != nil && answer == "" {
		return fmt.Errorf("reading confirmation: %w", ErrAborted)
	}

	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	default:
		return ErrAborted
	}
}
