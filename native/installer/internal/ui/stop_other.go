//go:build !unix

package ui

// StopProcessTree has no process tree to walk outside Unix; ctrl+c on a
// running step then cancels it, which ends its direct child only.
//
// shortcut: the installer is built and run on Linux and macOS; add a job
// object here if it ever runs on Windows.
func StopProcessTree() {}
