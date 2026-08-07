package blast

import (
	"io"
	"os"
	"strconv"

	"golang.org/x/term"
)

// displayWidth reports how many columns are available for table output, or 0
// when that is unknown (piped output, a buffer in tests) and rows should be
// printed in full rather than truncated to a guess.
func displayWidth(w io.Writer) int {
	if cols, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && cols > 0 {
		return cols
	}
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	width, _, err := term.GetSize(int(f.Fd()))
	if err != nil || width <= 0 {
		return 0
	}
	return width
}
