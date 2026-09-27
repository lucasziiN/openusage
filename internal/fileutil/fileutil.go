// Package fileutil holds file operations that must behave on Windows, where a
// file another process has open without the right share mode cannot be
// renamed onto, read, or replaced.
package fileutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to path through a temp file in the same
// directory and a rename, so concurrent readers see the old or the new
// content, never a partial write. The directory is created if missing.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	_, writeErr := tmp.Write(data)
	if writeErr == nil {
		writeErr = tmp.Sync()
	}
	closeErr := tmp.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr == nil {
		writeErr = os.Chmod(tmpPath, perm)
	}
	if writeErr == nil {
		writeErr = ReplaceFile(tmpPath, path)
	}
	if writeErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing %s: %w", path, writeErr)
	}
	return nil
}
