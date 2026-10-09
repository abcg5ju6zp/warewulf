package util

import (
	"os"
	"path/filepath"
)

// AtomicWriteFile writes data to filePath atomically. The content is first
// written to a temporary file in the same directory (created with perm),
// flushed to disk, and then renamed over filePath. On any failure the
// temporary file is removed and the original file, when present, is left
// untouched. The containing directory is synced after the rename so the
// new entry is persisted across crashes.
func AtomicWriteFile(filePath string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(filePath)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(filePath)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if err = tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}

	if err = os.Rename(tmpName, filePath); err != nil {
		return err
	}

	// Best effort: make the directory entry change durable. Syncing a
	// directory is not supported on every platform/filesystem, so a
	// failure here must not undo the successful rename.
	if dirFile, dirErr := os.Open(dir); dirErr == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}
