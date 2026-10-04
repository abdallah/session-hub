package cli

import (
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

// defaultWidth is the line width when stdout is not a terminal and COLUMNS
// is unset.
const defaultWidth = 100

// termWidth is stdout's width in columns.
func termWidth() int { return widthOf(os.Stdout.Fd(), os.Getenv) }

// widthOf is the terminal width of fd, else COLUMNS, else defaultWidth. The
// TIOCGWINSZ ioctl is the same on Linux and macOS, so it needs no other
// module.
func widthOf(fd uintptr, getenv func(string) string) int {
	var ws struct{ row, col, xpixel, ypixel uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno == 0 && ws.col > 0 {
		return int(ws.col)
	}
	if n, err := strconv.Atoi(getenv("COLUMNS")); err == nil && n > 0 {
		return n
	}
	return defaultWidth
}

// isTerminal reports whether fd is a terminal: the get-termios ioctl
// (ioctlGetTermios: TCGETS on Linux, TIOCGETA on macOS) succeeds.
func isTerminal(fd uintptr) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(ioctlGetTermios), uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

// termSize is stdout's width and height in cells, else termWidth and LINES,
// else 24 rows.
func termSize() (width, height int) {
	var ws struct{ row, col, xpixel, ypixel uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno == 0 && ws.col > 0 && ws.row > 0 {
		return int(ws.col), int(ws.row)
	}
	height = 24
	if n, err := strconv.Atoi(os.Getenv("LINES")); err == nil && n > 0 {
		height = n
	}
	return termWidth(), height
}
