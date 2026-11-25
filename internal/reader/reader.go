// Package reader reads drive temperatures with a timeout, without ever having
// more than one read in flight per drive.
//
// A drivetemp read can block for many seconds while a drive spins up. A read
// that has not returned by the timeout is reported as pending; its goroutine
// keeps running and its result is picked up by a later cycle.
package reader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/discovery"
	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/power"
)

// State is the outcome of one read attempt.
type State string

const (
	OK      State = "ok"
	Pending State = "pending"
	Failed  State = "failed"
	Asleep  State = "asleep"
)

// Result is the outcome for one drive.
type Result struct {
	State State
	// TempMC is the temperature in millidegrees Celsius, valid when State is OK.
	TempMC int
	Err    error
	// At is when the read finished, or for Pending when it started.
	At time.Time
}

// ReadFunc performs one blocking read of a drive. It sets State (OK, Failed
// or Asleep), TempMC and Err; the Reader sets At.
type ReadFunc func(d discovery.Drive) Result

// Plausible range for a drive temperature. Values outside it are firmware
// placeholders (0, 255, -1) rather than measurements and count as failed.
const (
	minPlausibleMC = 1_000
	maxPlausibleMC = 150_000
)

// ReadTemp reads and validates a drivetemp temp1_input file.
func ReadTemp(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}
	if v < minPlausibleMC || v > maxPlausibleMC {
		return 0, fmt.Errorf("implausible temperature %d m°C in %s", v, path)
	}
	return v, nil
}

// PowerCheckFunc returns a drive's power state; see package power.
type PowerCheckFunc func(devPath string) (power.State, error)

// FileReadFunc returns a ReadFunc that reads drivetemp files. When check is
// non-nil (--standby=skip), HDDs are first asked for their power state and
// drives in standby are reported Asleep without being read. If the power
// check fails, the temperature is read anyway. SSDs are never checked.
func FileReadFunc(devRoot string, check PowerCheckFunc) ReadFunc {
	return func(d discovery.Drive) Result {
		if check != nil && d.Class == discovery.HDD {
			st, err := check(devRoot + "/" + d.Block)
			if err == nil && st == power.StateStandby {
				return Result{State: Asleep}
			}
		}
		v, err := ReadTemp(d.TempPath)
		if err != nil {
			return Result{State: Failed, Err: err}
		}
		return Result{State: OK, TempMC: v}
	}
}

type flight struct {
	ch      chan Result
	started time.Time
}

// Reader runs reads for a set of drives. It is not safe for concurrent use.
type Reader struct {
	read     ReadFunc
	timeout  time.Duration
	now      func() time.Time
	inflight map[string]*flight
}

// New returns a Reader. now may be nil for time.Now.
func New(read ReadFunc, timeout time.Duration, now func() time.Time) *Reader {
	if now == nil {
		now = time.Now
	}
	return &Reader{read: read, timeout: timeout, now: now, inflight: map[string]*flight{}}
}

// Late is a result of a read that was pending in an earlier cycle and has
// finished since.
type Late struct {
	ID     string
	Result Result
}

// ReadAll reads every drive in parallel and waits at most the timeout.
// It returns one Result per drive ID, plus results of earlier pending reads
// that finished in the meantime (their drives are read again in this cycle).
func (r *Reader) ReadAll(ctx context.Context, drives []discovery.Drive) (map[string]Result, []Late) {
	var late []Late
	present := make(map[string]bool, len(drives))
	for _, d := range drives {
		present[d.ID] = true
	}
	for id, f := range r.inflight {
		select {
		case res := <-f.ch:
			delete(r.inflight, id)
			if present[id] {
				late = append(late, Late{ID: id, Result: res})
			}
		default:
		}
	}

	for _, d := range drives {
		if _, busy := r.inflight[d.ID]; busy {
			continue
		}
		f := &flight{ch: make(chan Result, 1), started: r.now()}
		r.inflight[d.ID] = f
		go func(d discovery.Drive) {
			var res Result
			defer func() {
				if p := recover(); p != nil {
					res = Result{State: Failed, Err: fmt.Errorf("read panicked: %v", p)}
				}
				res.At = r.now()
				f.ch <- res
			}()
			res = r.read(d)
		}(d)
	}

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	results := make(map[string]Result, len(drives))
	for _, d := range drives {
		f := r.inflight[d.ID]
		var res Result
		var done bool
		select {
		case res = <-f.ch:
			done = true
		case <-ctx.Done():
			select {
			case res = <-f.ch:
				done = true
			default:
			}
		}
		if done {
			delete(r.inflight, d.ID)
			results[d.ID] = res
			continue
		}
		results[d.ID] = Result{
			State: Pending,
			Err:   errors.New("read pending since " + f.started.Format(time.RFC3339)),
			At:    f.started,
		}
	}
	return results, late
}

// InFlight reports how many reads have not returned yet.
func (r *Reader) InFlight() int { return len(r.inflight) }
