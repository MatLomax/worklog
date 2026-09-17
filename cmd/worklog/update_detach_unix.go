//go:build !windows

package main

import "syscall"

// detachAttr starts the background updater in its own session, so it survives the
// short-lived parent exiting and is not killed by a terminal hangup.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
