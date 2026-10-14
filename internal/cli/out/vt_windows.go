package out

import (
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// enableConsoleVT turns on virtual-terminal processing for a console writer, so that colour codes show as colour. It
// reports false when the console refuses (the legacy conhost). A writer that is not a console needs nothing.
func enableConsoleVT(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return true
	}
	h := windows.Handle(f.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return true // not a console (a pipe or file): the TTY flag decides
	}
	return windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}
