package dataset

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

// Runner executes external commands. DuckDB has no pure-Go driver (the cgo one
// ships a 49 MB static library per platform), and internal/blast already shells
// out to the same CLI, so this stays a subprocess behind one interface that
// tests can replace.
type Runner interface {
	// Run streams the child's output to progress, for the multi-minute table build.
	Run(ctx context.Context, progress io.Writer, name string, args ...string) error
	// Output captures stdout, for commands whose result is parsed.
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, progress io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = progress
	cmd.Stderr = progress
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}

func (execRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return nil, fmt.Errorf("%s failed: %s", name, exitErr.Stderr)
		}
		return nil, fmt.Errorf("%s failed: %w", name, err)
	}
	return out, nil
}
