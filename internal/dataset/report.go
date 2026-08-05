package dataset

import (
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	// reportIndent is the left margin for everything printed inside a step.
	reportIndent = "      "
	// reportLabelColumn is where a field's value starts, measured from the margin.
	// It fits "destination", the longest label in use.
	reportLabelColumn = 13
)

// reporter prints a run's progress as numbered steps. It owns the numbering, so a
// step that does not run leaves no gap in the count, and the indentation, so the
// values inside a step line up in one column. Output is plain text with no escape
// sequences, which keeps it readable in a file or a CI log.
type reporter struct {
	out     io.Writer
	current int
	total   int
	start   time.Time
	// wrote records whether anything has been printed, so the header can keep its
	// blank line above it without opening the run on one.
	wrote bool
}

func newReporter(out io.Writer, totalSteps int) *reporter {
	return &reporter{out: out, total: totalSteps, start: time.Now()}
}

// header names the run above the first step. Each field is a "label value" pair
// already formatted by the caller.
func (r *reporter) header(command string, fields ...string) {
	if r.wrote {
		fmt.Fprintln(r.out)
	}
	r.wrote = true
	fmt.Fprintf(r.out, "%s\n", command)
	if len(fields) > 0 {
		fmt.Fprintf(r.out, "%s\n", strings.Join(fields, "   "))
	}
}

func (r *reporter) step(title string) {
	r.wrote = true
	r.current++
	fmt.Fprintf(r.out, "\n[%d/%d] %s\n", r.current, r.total, title)
}

// section starts a block that is not one of the numbered steps.
func (r *reporter) section(format string, args ...any) {
	r.wrote = true
	fmt.Fprintf(r.out, "\n%s\n", fmt.Sprintf(format, args...))
}

func (r *reporter) line(format string, args ...any) {
	r.wrote = true
	fmt.Fprintf(r.out, "%s%s\n", reportIndent, fmt.Sprintf(format, args...))
}

// field prints a labelled value, aligned with the other fields in the step.
func (r *reporter) field(label, format string, args ...any) {
	r.wrote = true
	fmt.Fprintf(r.out, "%s%-*s%s\n", reportIndent, reportLabelColumn, label, fmt.Sprintf(format, args...))
}

func (r *reporter) elapsed() time.Duration {
	return time.Since(r.start).Round(time.Second)
}

// writer exposes the underlying stream for output this package does not format
// itself, which is duckdb's.
func (r *reporter) writer() io.Writer { return r.out }

// heartbeatInterval is how often a long step reports that it is still going.
const heartbeatInterval = 30 * time.Second

// heartbeat prints message with elapsed time every interval until the returned
// function is called. That function waits for the printing goroutine, so no line
// can land after the caller has moved on to its next write.
func (r *reporter) heartbeat(message string, interval time.Duration) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		defer close(finished)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		start := time.Now()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				r.line("%s, %s elapsed", message, time.Since(start).Round(time.Second))
			}
		}
	}()

	return func() {
		close(done)
		<-finished
	}
}
