//go:build windows

package lightpanda

import (
	"os/exec"
	"syscall"
)

// hideWindow stops helper commands (wsl, docker, lightpanda) from flashing
// a console window on top of the Fyne UI.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
