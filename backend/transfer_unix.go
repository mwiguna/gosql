//go:build !windows

package main

import "os/exec"

func configureTransferCommand(cmd *exec.Cmd) {}
