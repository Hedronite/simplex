// Package outfile writes the probe document to a file so a reader never sees
// a partial one: a temp file in the same directory, fsync, atomic rename.
package outfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Mode is the permission of the finished file. It is set explicitly so the
// process umask cannot narrow or widen it.
const Mode os.FileMode = 0o644

// Write replaces path with data. On any failure the previous file, if any, is
// left untouched and no temp file remains.
func Write(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".simplex-out-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err = tmp.Chmod(Mode); err != nil {
		return fmt.Errorf("chmod %s: %w", name, err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err = os.Rename(name, path); err != nil {
		return fmt.Errorf("rename into %s: %w", path, err)
	}
	// Persist the rename. A directory that cannot be opened for sync is not a
	// failure of the write itself: the file is already in place.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
