package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/aggregate"
	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/config"
	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/discovery"
	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/reader"
	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/testutil"
)

type env struct {
	t     *testing.T
	sys   string
	out   string
	clock *testutil.Clock
	cfg   *config.Config
	logs  *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// newEnv parses args on top of defaults pointing at a writable fake sysfs
// copy. The USB drive sdd (55 °C, hotter than everything) is excluded unless
// args override --exclude handling.
func newEnv(t *testing.T, args ...string) *env {
	t.Helper()
	e := &env{t: t, sys: testutil.CopySysfs(t), out: filepath.Join(t.TempDir(), "out"), clock: testutil.NewClock(), logs: &syncBuffer{}}
	base := []string{"--sysfs-root", e.sys, "--output-dir", e.out, "--read-timeout", "50ms", "--interval", "100ms", "--log-level", "debug"}
	cfg, err := config.Parse("test", append(base, args...), func(string) string { return "" }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	e.cfg = cfg
	if err := os.MkdirAll(e.out, 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) daemon(read reader.ReadFunc) *Daemon {
	log := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return New(e.cfg, Deps{Now: e.clock.Now, Read: read, Logger: log})
}

func (e *env) file(name string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.out, name))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *env) setTemp(block, hwmon, content string) {
	testutil.WriteFile(e.t, e.sys, filepath.Join("block", block, "device", "hwmon", hwmon, "temp1_input"), content)
}

func (e *env) expect(results map[string]aggregate.Result, sensor, file string, mode aggregate.Mode) {
	e.t.Helper()
	if got := e.file(sensor); got != file {
		e.t.Errorf("%s file = %q, want %q (reason: %s)", sensor, got, file, results[sensor].Reason)
	}
	if results != nil && results[sensor].Mode != mode {
		e.t.Errorf("%s mode = %s, want %s (reason: %s)", sensor, results[sensor].Mode, mode, results[sensor].Reason)
	}
}

func TestStartWritesFailsafeBeforeFirstRead(t *testing.T) {
	e := newEnv(t, "--exclude", "Elements*")
	testutil.WriteFile(t, e.out, "hdd-max", "20000\n") // left over from an earlier run
	reads := 0
	d := e.daemon(func(discovery.Drive) reader.Result { reads++; return reader.Result{State: reader.OK, TempMC: 1000} })
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	if reads != 0 {
		t.Errorf("%d reads before failsafe was written", reads)
	}
	e.expect(nil, "hdd-max", "100000\n", "")
	e.expect(nil, "ssd-max", "100000\n", "")
}

func TestStartFailsWhenOutputUnwritable(t *testing.T) {
	e := newEnv(t)
	if err := os.Remove(e.out); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.out, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.daemon(nil).Start(); err == nil {
		t.Error("expected error")
	}
}

func TestCycleNormal(t *testing.T) {
	e := newEnv(t, "--exclude", "Elements*")
	d := e.daemon(nil)
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	res := d.Cycle(context.Background())
	// HDDs: sda 38 °C, sdb 41 °C (sdd 55 °C excluded). SSDs: sdc 35 °C, sdg 33 °C.
	e.expect(res, "hdd-max", "41000\n", aggregate.ModeNormal)
	e.expect(res, "ssd-max", "35000\n", aggregate.ModeNormal)
	if !strings.Contains(e.logs.String(), "sensor left failsafe") {
		t.Error("missing 'sensor left failsafe' log")
	}

	// Without the exclusion, the USB drive counts.
	e2 := newEnv(t)
	res = e2.daemon(nil).Cycle(context.Background())
	e2.expect(res, "hdd-max", "55000\n", aggregate.ModeNormal)
}

func TestCycleFollowsTemperatureAndDriveChanges(t *testing.T) {
	e := newEnv(t, "--exclude", "Elements*")
	d := e.daemon(nil)
	d.Cycle(context.Background())
	e.setTemp("sda", "hwmon2", "44000\n")
	res := d.Cycle(context.Background())
	e.expect(res, "hdd-max", "44000\n", aggregate.ModeNormal)

	// Hottest drive removed: max falls to the next one.
	if err := os.RemoveAll(filepath.Join(e.sys, "block", "sda")); err != nil {
		t.Fatal(err)
	}
	res = d.Cycle(context.Background())
	e.expect(res, "hdd-max", "41000\n", aggregate.ModeNormal)
	if !strings.Contains(e.logs.String(), "drive disappeared") {
		t.Error("missing 'drive disappeared' log")
	}
}

func TestCycleStaleReadings(t *testing.T) {
	e := newEnv(t, "--exclude", "Elements*")
	d := e.daemon(nil)
	res := d.Cycle(context.Background())
	e.expect(res, "hdd-max", "41000\n", aggregate.ModeNormal)

	// sdb (the hottest HDD) starts failing; its last reading stands in.
	e.setTemp("sdb", "hwmon3", "garbage\n")
	e.clock.Advance(30 * time.Second)
	res = d.Cycle(context.Background())
	e.expect(res, "hdd-max", "41000\n", aggregate.ModeNormal)

	e.clock.Advance(30 * time.Second) // exactly stale-after since the last good read
	res = d.Cycle(context.Background())
	e.expect(res, "hdd-max", "41000\n", aggregate.ModeNormal)

	e.clock.Advance(1 * time.Second)
	res = d.Cycle(context.Background())
	e.expect(res, "hdd-max", "100000\n", aggregate.ModeFailsafe)
	e.expect(res, "ssd-max", "35000\n", aggregate.ModeNormal)
	if !strings.Contains(e.logs.String(), "sensor entered failsafe") {
		t.Error("missing 'sensor entered failsafe' log")
	}

	e.setTemp("sdb", "hwmon3", "40000\n")
	res = d.Cycle(context.Background())
	e.expect(res, "hdd-max", "40000\n", aggregate.ModeNormal)
	if !strings.Contains(e.logs.String(), "drive recovered") {
		t.Error("missing 'drive recovered' log")
	}
}

func TestCyclePendingReads(t *testing.T) {
	e := newEnv(t, "--exclude", "Elements*")
	var mu sync.Mutex
	var gate chan struct{}
	starts := 0
	fileRead := reader.FileReadFunc("/dev", nil)
	read := func(drv discovery.Drive) reader.Result {
		if drv.Block == "sdb" {
			mu.Lock()
			g := gate
			starts++
			mu.Unlock()
			if g != nil {
				<-g
			}
		}
		return fileRead(drv)
	}
	d := e.daemon(read)
	res := d.Cycle(context.Background())
	e.expect(res, "hdd-max", "41000\n", aggregate.ModeNormal)

	// sdb blocks (spinning up): its last reading stands in while pending.
	mu.Lock()
	gate = make(chan struct{})
	mu.Unlock()
	e.clock.Advance(10 * time.Second)
	res = d.Cycle(context.Background())
	e.expect(res, "hdd-max", "41000\n", aggregate.ModeNormal)
	e.clock.Advance(10 * time.Second)
	res = d.Cycle(context.Background())
	e.expect(res, "hdd-max", "41000\n", aggregate.ModeNormal)
	mu.Lock()
	if starts != 2 {
		t.Errorf("sdb read started %d times, want 2 (no second read while pending)", starts)
	}
	mu.Unlock()

	// Pending longer than stale-after: failsafe.
	e.clock.Advance(45 * time.Second)
	res = d.Cycle(context.Background())
	e.expect(res, "hdd-max", "100000\n", aggregate.ModeFailsafe)
	if !strings.Contains(e.logs.String(), "drive read pending") {
		t.Error("missing 'drive read pending' log")
	}

	// The read returns; the next cycle uses fresh values again.
	e.setTemp("sdb", "hwmon3", "43000\n")
	mu.Lock()
	close(gate)
	gate = nil
	mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	e.clock.Advance(10 * time.Second)
	res = d.Cycle(context.Background())
	e.expect(res, "hdd-max", "43000\n", aggregate.ModeNormal)
}

func TestCyclePendingWithoutPreviousReadingIsFailsafe(t *testing.T) {
	e := newEnv(t, "--exclude", "Elements*")
	gate := make(chan struct{})
	defer close(gate)
	fileRead := reader.FileReadFunc("/dev", nil)
	d := e.daemon(func(drv discovery.Drive) reader.Result {
		if drv.Block == "sda" {
			<-gate
		}
		return fileRead(drv)
	})
	res := d.Cycle(context.Background())
	e.expect(res, "hdd-max", "100000\n", aggregate.ModeFailsafe)
	e.expect(res, "ssd-max", "35000\n", aggregate.ModeNormal)
}

func TestCycleMinDrives(t *testing.T) {
	e := newEnv(t, "--exclude", "Elements*", "--min-hdd", "3", "--min-ssd", "2")
	res := e.daemon(nil).Cycle(context.Background())
	e.expect(res, "hdd-max", "100000\n", aggregate.ModeFailsafe)
	e.expect(res, "ssd-max", "35000\n", aggregate.ModeNormal)
}

func TestCycleExcludedDrivesDoNotCountTowardsMinimum(t *testing.T) {
	e := newEnv(t, "--exclude", "ST*", "--failsafe-temp", "90")
	res := e.daemon(nil).Cycle(context.Background())
	// Only the USB drive sdd is left as HDD; with min-hdd 1 that is enough.
	e.expect(res, "hdd-max", "55000\n", aggregate.ModeNormal)

	e = newEnv(t, "--exclude", "ST*", "--exclude", "Elements*", "--failsafe-temp", "90")
	res = e.daemon(nil).Cycle(context.Background())
	e.expect(res, "hdd-max", "90000\n", aggregate.ModeFailsafe)
}

func TestDisabledSensorWritesNoFile(t *testing.T) {
	e := newEnv(t, "--exclude", "Elements*", "--sensors", "hdd-max")
	d := e.daemon(nil)
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	res := d.Cycle(context.Background())
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if _, ok := res["ssd-max"]; ok {
		t.Error("ssd-max computed while disabled")
	}
	if _, err := os.Stat(filepath.Join(e.out, "ssd-max")); !os.IsNotExist(err) {
		t.Errorf("ssd-max file exists: %v", err)
	}
}

func TestEnabledSensorWithoutDrivesIsFailsafe(t *testing.T) {
	e := newEnv(t, "--exclude", "Elements*", "--min-ssd", "0")
	for _, b := range []string{"sdc", "sdg"} {
		if err := os.RemoveAll(filepath.Join(e.sys, "block", b)); err != nil {
			t.Fatal(err)
		}
	}
	res := e.daemon(nil).Cycle(context.Background())
	e.expect(res, "ssd-max", "100000\n", aggregate.ModeFailsafe)
	e.expect(res, "hdd-max", "41000\n", aggregate.ModeNormal)
}

func TestDiscoveryFailureIsFailsafe(t *testing.T) {
	e := newEnv(t)
	if err := os.RemoveAll(filepath.Join(e.sys, "block")); err != nil {
		t.Fatal(err)
	}
	res := e.daemon(nil).Cycle(context.Background())
	e.expect(res, "hdd-max", "100000\n", aggregate.ModeFailsafe)
	e.expect(res, "ssd-max", "100000\n", aggregate.ModeFailsafe)
}

func TestWriteFailureIsFailsafe(t *testing.T) {
	e := newEnv(t, "--exclude", "Elements*")
	// hdd-max cannot be replaced: a non-empty directory is in the way.
	testutil.WriteFile(t, e.out, "hdd-max/blocker", "")
	res := e.daemon(nil).Cycle(context.Background())
	if r := res["hdd-max"]; r.Mode != aggregate.ModeFailsafe || !strings.Contains(r.Reason, "write failed") {
		t.Errorf("hdd-max = %+v", r)
	}
	e.expect(res, "ssd-max", "35000\n", aggregate.ModeNormal)
}

func TestReadPanicIsTreatedAsFailedRead(t *testing.T) {
	e := newEnv(t, "--exclude", "Elements*")
	res := e.daemon(func(discovery.Drive) reader.Result { panic("boom") }).Cycle(context.Background())
	e.expect(res, "hdd-max", "100000\n", aggregate.ModeFailsafe)
	e.expect(res, "ssd-max", "100000\n", aggregate.ModeFailsafe)
}

func TestStandbySkip(t *testing.T) {
	e := newEnv(t, "--exclude", "Elements*", "--standby", "skip", "--standby-temp", "25")
	hddAsleep := true
	fileRead := reader.FileReadFunc("/dev", nil)
	d := e.daemon(func(drv discovery.Drive) reader.Result {
		if drv.Class == discovery.HDD && hddAsleep {
			return reader.Result{State: reader.Asleep}
		}
		return fileRead(drv)
	})
	res := d.Cycle(context.Background())
	e.expect(res, "hdd-max", "25000\n", aggregate.ModeStandby)
	e.expect(res, "ssd-max", "35000\n", aggregate.ModeNormal)
	if !strings.Contains(e.logs.String(), "drive asleep") {
		t.Error("missing 'drive asleep' log")
	}
	hddAsleep = false
	res = d.Cycle(context.Background())
	e.expect(res, "hdd-max", "41000\n", aggregate.ModeNormal)
}

func TestShutdown(t *testing.T) {
	for _, tt := range []struct {
		onExit, want string
	}{{"failsafe", "100000\n"}, {"keep", "41000\n"}} {
		t.Run(tt.onExit, func(t *testing.T) {
			e := newEnv(t, "--exclude", "Elements*", "--on-exit", tt.onExit)
			d := e.daemon(nil)
			if err := d.Start(); err != nil {
				t.Fatal(err)
			}
			d.Cycle(context.Background())
			if err := d.Shutdown(); err != nil {
				t.Fatal(err)
			}
			e.expect(nil, "hdd-max", tt.want, "")
		})
	}
}

func TestRunStopsWithFailsafe(t *testing.T) {
	e := newEnv(t, "--exclude", "Elements*")
	d := New(e.cfg, Deps{Logger: slog.New(slog.NewTextHandler(e.logs, nil))})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitForFile(t, filepath.Join(e.out, "hdd-max"), "41000\n")
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	e.expect(nil, "hdd-max", "100000\n", "")
	e.expect(nil, "ssd-max", "100000\n", "")
}

func waitForFile(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && string(b) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	b, _ := os.ReadFile(path)
	t.Fatalf("%s = %q, want %q", path, b, want)
}

func TestStatusFile(t *testing.T) {
	e := newEnv(t, "--exclude", "Elements*", "--status-file", filepath.Join(t.TempDir(), "status.json"))
	e.daemon(nil).Cycle(context.Background())
	b, err := os.ReadFile(e.cfg.StatusFile)
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Drives) != 4 || len(st.Sensors) != 2 {
		t.Fatalf("status = %s", b)
	}
	for _, d := range st.Drives {
		if d.State != "ok" || d.TempMC == nil || d.ID == "" || d.Class == "" {
			t.Errorf("drive status = %+v", d)
		}
	}
	if st.Sensors[0].Name != "hdd-max" || st.Sensors[0].ValueMC != 41000 || st.Sensors[0].Mode != "normal" {
		t.Errorf("sensor status = %+v", st.Sensors[0])
	}
}

func TestLogsOncePerStateChange(t *testing.T) {
	e := newEnv(t, "--exclude", "Elements*")
	d := e.daemon(nil)
	for i := 0; i < 3; i++ {
		d.Cycle(context.Background())
	}
	logs := e.logs.String()
	if n := strings.Count(logs, "drive appeared"); n != 4 {
		t.Errorf("'drive appeared' logged %d times, want 4", n)
	}
	if n := strings.Count(logs, "drive excluded"); n != 1 {
		t.Errorf("'drive excluded' logged %d times, want 1", n)
	}
	if n := strings.Count(logs, "level=INFO msg=\"sensor"); n != 2 {
		t.Errorf("sensor info logged %d times, want 2:\n%s", n, logs)
	}
}

// perSensorEnv sets different failsafe values for the two sensors.
func perSensorEnv(t *testing.T, args ...string) *env {
	return newEnv(t, append([]string{"--exclude", "Elements*", "--hdd-failsafe-temp", "55", "--ssd-failsafe-temp", "70"}, args...)...)
}

func TestPerSensorFailsafeOnStartAndShutdown(t *testing.T) {
	e := perSensorEnv(t)
	d := e.daemon(nil)
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	e.expect(nil, "hdd-max", "55000\n", "")
	e.expect(nil, "ssd-max", "70000\n", "")
	if !strings.Contains(e.logs.String(), `failsafe_temp="map[hdd-max:55 ssd-max:70]"`) {
		t.Errorf("start log does not show per-sensor failsafe:\n%s", e.logs.String())
	}
	res := d.Cycle(context.Background())
	e.expect(res, "hdd-max", "41000\n", aggregate.ModeNormal)
	e.expect(res, "ssd-max", "35000\n", aggregate.ModeNormal)
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	e.expect(nil, "hdd-max", "55000\n", "")
	e.expect(nil, "ssd-max", "70000\n", "")
}

func TestPerSensorFailsafeInCycle(t *testing.T) {
	t.Run("read failure", func(t *testing.T) {
		e := perSensorEnv(t)
		res := e.daemon(func(discovery.Drive) reader.Result { return reader.Result{State: reader.Failed} }).Cycle(context.Background())
		e.expect(res, "hdd-max", "55000\n", aggregate.ModeFailsafe)
		e.expect(res, "ssd-max", "70000\n", aggregate.ModeFailsafe)
	})
	t.Run("too few drives", func(t *testing.T) {
		e := perSensorEnv(t, "--min-hdd", "5")
		res := e.daemon(nil).Cycle(context.Background())
		e.expect(res, "hdd-max", "55000\n", aggregate.ModeFailsafe)
		e.expect(res, "ssd-max", "35000\n", aggregate.ModeNormal)
	})
	t.Run("discovery failure", func(t *testing.T) {
		e := perSensorEnv(t)
		if err := os.RemoveAll(filepath.Join(e.sys, "block")); err != nil {
			t.Fatal(err)
		}
		res := e.daemon(nil).Cycle(context.Background())
		e.expect(res, "hdd-max", "55000\n", aggregate.ModeFailsafe)
		e.expect(res, "ssd-max", "70000\n", aggregate.ModeFailsafe)
	})
	t.Run("internal error", func(t *testing.T) {
		e := perSensorEnv(t)
		res := e.daemon(func(discovery.Drive) reader.Result { panic("boom") }).Cycle(context.Background())
		e.expect(res, "hdd-max", "55000\n", aggregate.ModeFailsafe)
		e.expect(res, "ssd-max", "70000\n", aggregate.ModeFailsafe)
	})
}

func TestStatusFileShowsFailsafe(t *testing.T) {
	e := perSensorEnv(t, "--status-file", filepath.Join(t.TempDir(), "status.json"))
	e.daemon(nil).Cycle(context.Background())
	b, err := os.ReadFile(e.cfg.StatusFile)
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, s := range st.Sensors {
		got[s.Name] = s.FailsafeMC
	}
	if got["hdd-max"] != 55000 || got["ssd-max"] != 70000 {
		t.Errorf("failsafe in status = %v", got)
	}
}
