package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/testutil"
)

func noEnv(string) string { return "" }

func TestVersion(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		var out strings.Builder
		if code := run(args, noEnv, &out, &out); code != 0 || !strings.Contains(out.String(), "drivetemp-sensor dev") {
			t.Errorf("%v: code %d, output %q", args, code, out.String())
		}
	}
}

func TestBadFlagsExit2(t *testing.T) {
	var out strings.Builder
	if code := run([]string{"--standby", "maybe"}, noEnv, &out, &out); code != 2 {
		t.Errorf("code = %d", code)
	}
	if code := run([]string{"--help"}, noEnv, &out, &out); code != 0 {
		t.Errorf("--help code = %d", code)
	}
}

func TestHealthcheck(t *testing.T) {
	dir := t.TempDir()
	var out strings.Builder
	args := []string{"healthcheck", "--output-dir", dir, "--interval", "10s"}
	if code := run(args, noEnv, &out, &out); code != 1 {
		t.Errorf("missing files: code = %d", code)
	}
	testutil.WriteFile(t, dir, "hdd-max", "41000\n")
	testutil.WriteFile(t, dir, "ssd-max", "35000\n")
	if code := run(args, noEnv, &out, &out); code != 0 {
		t.Errorf("fresh files: code = %d, output %s", code, out.String())
	}
	old := time.Now().Add(-31 * time.Second)
	if err := os.Chtimes(filepath.Join(dir, "ssd-max"), old, old); err != nil {
		t.Fatal(err)
	}
	if code := run(args, noEnv, &out, &out); code != 1 {
		t.Errorf("stale ssd-max: code = %d", code)
	}
	// Environment configures the healthcheck like the daemon (as in the image).
	env := func(k string) string {
		return map[string]string{"DRIVETEMP_SENSOR_OUTPUT_DIR": dir, "DRIVETEMP_SENSOR_SENSORS": "hdd-max"}[k]
	}
	if code := run([]string{"healthcheck"}, env, &out, &out); code != 0 {
		t.Errorf("env-configured, ssd-max disabled: code = %d", code)
	}
}

// TestSIGTERMWritesFailsafe runs the daemon and sends the process SIGTERM.
func TestSIGTERMWritesFailsafe(t *testing.T) {
	sys := testutil.CopySysfs(t)
	out := filepath.Join(t.TempDir(), "out")
	args := []string{"--sysfs-root", sys, "--output-dir", out, "--interval", "50ms", "--read-timeout", "50ms", "--exclude", "Elements*"}
	var logs strings.Builder
	done := make(chan int, 1)
	go func() { done <- run(args, noEnv, &logs, &logs) }()

	hdd := filepath.Join(out, "hdd-max")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, _ := os.ReadFile(hdd); string(b) == "41000\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon never wrote a reading")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop on SIGTERM")
	}
	if b, _ := os.ReadFile(hdd); string(b) != "100000\n" {
		t.Errorf("hdd-max after SIGTERM = %q", b)
	}
}
