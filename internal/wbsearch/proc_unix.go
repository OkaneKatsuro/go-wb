//go:build unix

package wbsearch

import (
	"os/exec"
	"syscall"
)

// ownGroup запускает подпроцесс в своей группе процессов, чтобы остановка
// накрыла и его потомков: при `go run` сам wb-search — внук, и сигнал только
// родителю оставил бы его вместе с браузером.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminate шлёт SIGTERM всей группе процессов команды.
func terminate(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}
