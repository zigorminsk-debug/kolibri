//go:build !windows

package lightpanda

import "os/exec"

func hideWindow(*exec.Cmd) {}
