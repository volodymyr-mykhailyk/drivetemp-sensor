// Package testutil holds helpers shared by tests.
package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// SysfsDir returns the read-only fake sysfs tree in testdata/sysfs.
func SysfsDir(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate testutil source")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "sysfs")
}

// CopySysfs returns a writable copy of the fake sysfs tree.
func CopySysfs(t testing.TB) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "sys")
	if err := os.CopyFS(dst, os.DirFS(SysfsDir(t))); err != nil {
		t.Fatal(err)
	}
	return dst
}

// WriteFile writes content to root/rel, creating directories.
func WriteFile(t testing.TB, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ReadFile returns the content of root/rel, failing the test on error.
func ReadFile(t testing.TB, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Clock is a manually advanced clock, safe for concurrent use.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a clock set to a fixed instant.
func NewClock() *Clock {
	return &Clock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

// Now returns the current fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
