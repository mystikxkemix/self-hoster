// Package fileutil holds small file helpers shared by the persistence packages.
package fileutil

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to path via a temporary file and a rename, so readers
// never see a partial file. The file is private to the owner (it may hold secrets).
func WriteFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
