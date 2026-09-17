//go:build windows

package main

import "syscall"

// detachAttr starts the background updater fully detached from the parent's
// console (DETACHED_PROCESS) and in a new process group (CREATE_NEW_PROCESS_GROUP),
// so it survives the short-lived parent exiting.
func detachAttr() *syscall.SysProcAttr {
	const (
		detachedProcess       = 0x00000008
		createNewProcessGroup = 0x00000200
	)
	return &syscall.SysProcAttr{CreationFlags: detachedProcess | createNewProcessGroup}
}
