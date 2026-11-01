// Package fsutil holds small file helpers shared by the state-dir writers
// (cache, session, digest cursor).
package fsutil

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic replaces path with data (mode 0600), creating the parent
// directory (0700) if needed. The data goes to a uniquely named temporary
// file in the same directory, is flushed to disk, then renamed over path, so
// readers never see a partial file and concurrent writers (serve, digest and
// check sharing one state dir) never clobber each other's temporary file.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	// CreateTemp already uses 0600; Chmod keeps that true whatever the umask.
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}
