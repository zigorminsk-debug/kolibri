//go:build !windows

package screenshot

import "os/exec"

func hideWindow(*exec.Cmd) {}
