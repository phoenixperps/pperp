package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableColor turns on the console's ANSI colours (off in older consoles until asked for).
func enableColor() bool {
	h := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return false
	}
	return windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}
