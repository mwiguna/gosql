package main

import (
	"os/exec"
	"syscall"
)

func configureTransferCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
