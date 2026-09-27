//go:build !windows

package fileutil

import "os"

// ReplaceFile renames tmp onto final. On Unix this is atomic even while a
// reader holds final open, so no retry is needed.
func ReplaceFile(tmp, final string) error {
	return os.Rename(tmp, final)
}

// ReadFile reads path. On Unix a concurrent rename onto path never blocks the
// read, so no retry is needed.
func ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// OpenShared opens path read-only. Unix never blocks renames or deletes of
// open files, so this is os.Open.
func OpenShared(path string) (*os.File, error) {
	return os.Open(path)
}
