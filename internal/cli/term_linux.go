package cli

import "syscall"

// ioctlGetTermios reads a terminal's settings.
const ioctlGetTermios = syscall.TCGETS
