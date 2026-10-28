//go:build !unix

package wbsearch

import "os/exec"

func ownGroup(*exec.Cmd) {}

func terminate(cmd *exec.Cmd) error { return cmd.Process.Kill() }
