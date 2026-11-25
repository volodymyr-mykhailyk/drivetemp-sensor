package output

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFormatMC(t *testing.T) {
	if got := string(FormatMC(41000)); got != "41000\n" {
		t.Errorf("FormatMC = %q", got)
	}
}

func TestWriteSensorIsAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSensor(dir, "hdd-max", 40000); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "hdd-max")
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	// A consumer holding the old file open keeps reading the old content.
	old, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()

	if err := WriteSensor(dir, "hdd-max", 41000); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("file was rewritten in place, not replaced by rename")
	}
	if b, _ := io.ReadAll(old); string(b) != "40000\n" {
		t.Errorf("old handle reads %q, want the complete old content", b)
	}
	if b, _ := os.ReadFile(p); string(b) != "41000\n" {
		t.Errorf("content = %q", b)
	}
	if m := after.Mode().Perm(); m != 0o644 {
		t.Errorf("mode = %o, want 644", m)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("leftover files: %v", names)
	}
}

func TestWriteAtomicFailureLeavesTargetAndNoTemp(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSensor(filepath.Join(dir, "missing"), "hdd-max", 1); err == nil {
		t.Error("expected error for missing directory")
	}
	// Target is a non-empty directory: rename fails, temp file is cleaned up.
	if err := os.MkdirAll(filepath.Join(dir, "hdd-max", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteSensor(dir, "hdd-max", 1); err == nil {
		t.Error("expected error when target is a directory")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestCheck(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	sensors := []string{"hdd-max", "ssd-max"}
	if err := Check(dir, sensors, 30*time.Second, now); err == nil {
		t.Error("missing files: expected error")
	}
	for _, s := range sensors {
		if err := WriteSensor(dir, s, 40000); err != nil {
			t.Fatal(err)
		}
	}
	if err := Check(dir, sensors, 30*time.Second, now); err != nil {
		t.Errorf("fresh files: %v", err)
	}
	old := now.Add(-31 * time.Second)
	if err := os.Chtimes(filepath.Join(dir, "ssd-max"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := Check(dir, sensors, 30*time.Second, now); err == nil || !strings.Contains(err.Error(), "ssd-max") {
		t.Errorf("stale file: err = %v", err)
	}
	if err := Check(dir, []string{"hdd-max"}, 30*time.Second, now); err != nil {
		t.Errorf("only checks enabled sensors: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hdd-max"), []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Check(dir, []string{"hdd-max"}, 30*time.Second, now); err == nil {
		t.Error("garbage content: expected error")
	}
}
