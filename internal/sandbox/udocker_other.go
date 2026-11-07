//go:build !unix

package sandbox

import "os/exec"

func configureProcessGroup(*exec.Cmd) {}
