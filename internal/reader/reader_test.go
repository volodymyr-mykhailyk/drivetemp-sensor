package reader

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/discovery"
	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/power"
	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/testutil"
)

func TestReadTemp(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		content string
		want    int
		err     bool
	}{
		{"41000\n", 41000, false},
		{" 38000 ", 38000, false},
		{"1000\n", 1000, false},
		{"150000\n", 150000, false},
		{"abc\n", 0, true},
		{"\n", 0, true},
		{"0\n", 0, true},      // firmware placeholder
		{"-1000\n", 0, true},  // implausible
		{"255000\n", 0, true}, // firmware placeholder
		{"150001\n", 0, true}, // implausible
	}
	for i, tt := range tests {
		name := filepath.Join("t", string(rune('a'+i)))
		testutil.WriteFile(t, dir, name, tt.content)
		got, err := ReadTemp(filepath.Join(dir, name))
		if got != tt.want || (err != nil) != tt.err {
			t.Errorf("ReadTemp(%q) = %d, %v; want %d, err=%v", tt.content, got, err, tt.want, tt.err)
		}
	}
	if _, err := ReadTemp(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file: expected error")
	}
}

func TestFileReadFuncStandby(t *testing.T) {
	dir := t.TempDir()
	testutil.WriteFile(t, dir, "temp", "40000\n")
	hdd := discovery.Drive{ID: "h", Block: "sda", Class: discovery.HDD, TempPath: filepath.Join(dir, "temp")}
	ssd := discovery.Drive{ID: "s", Block: "sdb", Class: discovery.SSD, TempPath: filepath.Join(dir, "temp")}
	asleepHDD := hdd
	asleepHDD.TempPath = filepath.Join(dir, "does-not-exist") // reading it would fail

	var checked []string
	check := func(st power.State, err error) PowerCheckFunc {
		return func(dev string) (power.State, error) {
			checked = append(checked, dev)
			return st, err
		}
	}

	t.Run("read mode does not check", func(t *testing.T) {
		checked = nil
		r := FileReadFunc("/dev", nil)(hdd)
		if r.State != OK || r.TempMC != 40000 || len(checked) != 0 {
			t.Errorf("got %+v, checked %v", r, checked)
		}
	})
	t.Run("standby HDD is not read", func(t *testing.T) {
		checked = nil
		r := FileReadFunc("/dev", check(power.StateStandby, nil))(asleepHDD)
		if r.State != Asleep || len(checked) != 1 || checked[0] != "/dev/sda" {
			t.Errorf("got %+v, checked %v", r, checked)
		}
	})
	t.Run("active HDD is read", func(t *testing.T) {
		checked = nil
		r := FileReadFunc("/dev", check(power.StateActive, nil))(hdd)
		if r.State != OK || r.TempMC != 40000 || len(checked) != 1 {
			t.Errorf("got %+v, checked %v", r, checked)
		}
	})
	t.Run("failed power check falls back to reading", func(t *testing.T) {
		checked = nil
		r := FileReadFunc("/dev", check(power.StateUnknown, errors.New("EPERM")))(hdd)
		if r.State != OK || r.TempMC != 40000 || len(checked) != 1 {
			t.Errorf("got %+v, checked %v", r, checked)
		}
	})
	t.Run("SSD is never checked", func(t *testing.T) {
		checked = nil
		r := FileReadFunc("/dev", check(power.StateStandby, nil))(ssd)
		if r.State != OK || len(checked) != 0 {
			t.Errorf("got %+v, checked %v", r, checked)
		}
	})
	t.Run("failed read", func(t *testing.T) {
		r := FileReadFunc("/dev", nil)(asleepHDD)
		if r.State != Failed || r.Err == nil {
			t.Errorf("got %+v", r)
		}
	})
}

// blockingReads is a ReadFunc where chosen drives block until released.
type blockingReads struct {
	mu    sync.Mutex
	block map[string]chan struct{}
	calls map[string]int
	temps map[string]int
}

func newBlockingReads() *blockingReads {
	return &blockingReads{block: map[string]chan struct{}{}, calls: map[string]int{}, temps: map[string]int{}}
}

func (b *blockingReads) read(d discovery.Drive) Result {
	b.mu.Lock()
	b.calls[d.ID]++
	ch := b.block[d.ID]
	v := b.temps[d.ID]
	b.mu.Unlock()
	if ch != nil {
		<-ch
	}
	if d.ID == "panic" {
		panic("boom")
	}
	return Result{State: OK, TempMC: v}
}

func (b *blockingReads) callCount(id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls[id]
}

func TestReadAllPendingRead(t *testing.T) {
	b := newBlockingReads()
	release := make(chan struct{})
	b.block["slow"] = release
	b.temps["slow"] = 45000
	b.temps["fast"] = 40000
	clock := testutil.NewClock()
	r := New(b.read, 20*time.Millisecond, clock.Now)
	drives := []discovery.Drive{{ID: "fast"}, {ID: "slow"}}

	res, late := r.ReadAll(context.Background(), drives)
	if res["fast"].State != OK || res["fast"].TempMC != 40000 {
		t.Errorf("fast = %+v", res["fast"])
	}
	if res["slow"].State != Pending || !res["slow"].At.Equal(clock.Now()) {
		t.Errorf("slow = %+v, want pending since start", res["slow"])
	}
	if len(late) != 0 {
		t.Errorf("late = %v", late)
	}

	// Second cycle: the slow read is still blocked; no second read starts.
	clock.Advance(10 * time.Second)
	res, _ = r.ReadAll(context.Background(), drives)
	if res["slow"].State != Pending {
		t.Errorf("slow = %+v, want pending", res["slow"])
	}
	if n := b.callCount("slow"); n != 1 {
		t.Errorf("slow read started %d times, want 1", n)
	}
	if n := b.callCount("fast"); n != 2 {
		t.Errorf("fast read started %d times, want 2", n)
	}
	if r.InFlight() != 1 {
		t.Errorf("in flight = %d", r.InFlight())
	}

	// The read returns between cycles: reported as late, and read again.
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for !r.doneFor("slow") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	b.mu.Lock()
	delete(b.block, "slow")
	b.mu.Unlock()
	clock.Advance(10 * time.Second)
	res, late = r.ReadAll(context.Background(), drives)
	if len(late) != 1 || late[0].ID != "slow" || late[0].Result.State != OK || late[0].Result.TempMC != 45000 {
		t.Errorf("late = %+v", late)
	}
	if res["slow"].State != OK || res["slow"].TempMC != 45000 {
		t.Errorf("slow = %+v, want fresh OK", res["slow"])
	}
	if n := b.callCount("slow"); n != 2 {
		t.Errorf("slow read started %d times, want 2", n)
	}
	if r.InFlight() != 0 {
		t.Errorf("in flight = %d", r.InFlight())
	}
}

// doneFor reports whether the in-flight read for id has a result waiting.
func (r *Reader) doneFor(id string) bool {
	f, ok := r.inflight[id]
	return ok && len(f.ch) == 1
}

func TestReadAllPanicIsFailure(t *testing.T) {
	b := newBlockingReads()
	r := New(b.read, time.Second, nil)
	res, _ := r.ReadAll(context.Background(), []discovery.Drive{{ID: "panic"}})
	if res["panic"].State != Failed || res["panic"].Err == nil {
		t.Errorf("got %+v", res["panic"])
	}
}

func TestReadAllCancelledContext(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	defer close(release)
	read := func(discovery.Drive) Result {
		calls.Add(1)
		<-release
		return Result{State: OK, TempMC: 1}
	}
	r := New(read, time.Hour, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	res, _ := r.ReadAll(ctx, []discovery.Drive{{ID: "a"}})
	if res["a"].State != Pending {
		t.Errorf("got %+v", res["a"])
	}
	if time.Since(start) > time.Second {
		t.Error("ReadAll did not return promptly on a cancelled context")
	}
}

func TestReadAllForgetsVanishedDrives(t *testing.T) {
	b := newBlockingReads()
	release := make(chan struct{})
	b.block["gone"] = release
	r := New(b.read, 10*time.Millisecond, nil)
	r.ReadAll(context.Background(), []discovery.Drive{{ID: "gone"}})
	close(release)
	for !r.doneFor("gone") {
		time.Sleep(time.Millisecond)
	}
	_, late := r.ReadAll(context.Background(), []discovery.Drive{{ID: "other"}})
	if len(late) != 0 {
		t.Errorf("late result for a vanished drive: %+v", late)
	}
	if r.InFlight() != 0 {
		t.Errorf("in flight = %d", r.InFlight())
	}
}
