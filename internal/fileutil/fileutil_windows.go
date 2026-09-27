//go:build windows

package fileutil

import (
	"errors"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// Windows file-sharing errors that clear within milliseconds: a concurrent
// reader holding the target open, or a reader opening a file mid-rename.
const (
	errAccessDenied     = syscall.Errno(5)  // ERROR_ACCESS_DENIED
	errSharingViolation = syscall.Errno(32) // ERROR_SHARING_VIOLATION
)

func transientWindowsErr(err error) bool {
	return errors.Is(err, errAccessDenied) || errors.Is(err, errSharingViolation)
}

// retryTransient runs op up to ~20 times with a short linear backoff, retrying
// only on transient Windows sharing/lock errors. The total worst-case wait is
// ~210ms, far longer than the sub-millisecond window a concurrent reader keeps
// a small file open.
func retryTransient(op func() error) error {
	var err error
	for i := range 20 {
		if err = op(); err == nil || !transientWindowsErr(err) {
			return err
		}
		time.Sleep(time.Duration(i+1) * time.Millisecond)
	}
	return err
}

// ReplaceFile renames tmp onto final, retrying when a concurrent reader
// momentarily holds final open.
func ReplaceFile(tmp, final string) error {
	return retryTransient(func() error { return os.Rename(tmp, final) })
}

// ReadFile reads path, retrying when a concurrent atomic replace momentarily
// blocks the open.
func ReadFile(path string) ([]byte, error) {
	var data []byte
	err := retryTransient(func() error {
		var e error
		data, e = os.ReadFile(path)
		return e
	})
	return data, err
}

// OpenShared opens path read-only while letting other processes delete,
// rename or replace it. os.Open omits FILE_SHARE_DELETE, so while it holds a
// file another program's atomic temp-file-and-rename save of that file fails.
func OpenShared(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}
