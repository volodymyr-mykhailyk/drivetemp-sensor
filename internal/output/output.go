// Package output writes sensor files atomically and checks their freshness.
package output

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// FileMode is the permission of every file this tool writes.
const FileMode = 0o644

// WriteAtomic replaces dir/name with data: it writes a temporary file in the
// same directory, fsyncs it and renames it over the target, so a reader sees
// either the old or the new content, never a partial write.
func WriteAtomic(dir, name string, data []byte) (err error) {
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Chmod(FileMode); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	// Persist the rename; best effort, not every filesystem supports it.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// FormatMC renders a sensor value in the hwmon temp*_input format.
func FormatMC(mc int) []byte {
	return []byte(strconv.Itoa(mc) + "\n")
}

// WriteSensor writes one sensor value.
func WriteSensor(dir, name string, mc int) error {
	return WriteAtomic(dir, name, FormatMC(mc))
}

// Check verifies that every named sensor file in dir exists, holds an integer
// and was modified within maxAge of now. It is the healthcheck.
func Check(dir string, sensors []string, maxAge time.Duration, now time.Time) error {
	var errs []error
	for _, s := range sensors {
		p := filepath.Join(dir, s)
		st, err := os.Stat(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if age := now.Sub(st.ModTime()); age > maxAge {
			errs = append(errs, fmt.Errorf("%s last written %s ago (limit %s)", p, age.Round(time.Second), maxAge))
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := strconv.Atoi(strings.TrimSpace(string(b))); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}
