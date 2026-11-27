// Package fsutil holds small filesystem helpers shared by the app.
package fsutil

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to path so that a power cut or crash leaves
// either the old file or the new one, never an empty or half-written one:
// it writes a temporary file in the same directory, syncs it to disk, then
// renames it over path (atomic on the same filesystem) and syncs the
// directory so the rename itself survives. os.WriteFile truncates path
// first, which on a device losing power mid-write leaves a broken file.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()

	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmp, perm); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync() // best effort: not every filesystem supports it
		_ = d.Close()
	}
	return nil
}
