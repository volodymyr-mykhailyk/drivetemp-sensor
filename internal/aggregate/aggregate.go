// Package aggregate turns per-drive readings into one sensor value.
//
// The rules favour a hot answer whenever the temperature is not known: a
// fan controller reading the value then runs the fans fast instead of slow.
package aggregate

import (
	"fmt"
	"time"

	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/reader"
)

// Reading is a temperature at a point in time.
type Reading struct {
	TempMC int
	At     time.Time
}

// Sample is one drive's input to the aggregation.
type Sample struct {
	ID    string
	State reader.State
	// TempMC is the fresh reading; valid when State is OK.
	TempMC int
	// LastGood is the most recent successful reading before this cycle, if any.
	LastGood *Reading
}

// Params configures the aggregation.
type Params struct {
	Now        time.Time
	StaleAfter time.Duration
	FailsafeMC int
	StandbyMC  int
	// MinDrives is the number of drives below which the sensor fails safe.
	MinDrives int
}

// Mode says how a value was produced.
type Mode string

const (
	ModeNormal   Mode = "normal"
	ModeStandby  Mode = "standby"
	ModeFailsafe Mode = "failsafe"
)

// Result is a sensor value and why it has that value.
type Result struct {
	ValueMC int
	Mode    Mode
	Reason  string
}

// Failsafe returns a failsafe result with the given reason.
func Failsafe(p Params, reason string) Result {
	return Result{ValueMC: p.FailsafeMC, Mode: ModeFailsafe, Reason: reason}
}

// Max aggregates samples of one drive class:
//
//   - fewer drives than MinDrives, or none at all: failsafe;
//   - a pending or failed drive whose last good reading is younger than
//     StaleAfter contributes that reading; otherwise the sensor fails safe;
//   - all drives asleep: StandbyMC;
//   - otherwise the max of fresh and still-valid readings (asleep drives do
//     not contribute).
func Max(samples []Sample, p Params) Result {
	if len(samples) == 0 {
		return Failsafe(p, "no drives found")
	}
	if len(samples) < p.MinDrives {
		return Failsafe(p, fmt.Sprintf("found %d drives, need at least %d", len(samples), p.MinDrives))
	}
	maxMC, fresh, stale, asleep := 0, 0, 0, 0
	have := false
	take := func(v int) {
		if !have || v > maxMC {
			maxMC = v
		}
		have = true
	}
	for _, s := range samples {
		switch s.State {
		case reader.OK:
			fresh++
			take(s.TempMC)
		case reader.Asleep:
			asleep++
		case reader.Pending, reader.Failed:
			if s.LastGood == nil {
				return Failsafe(p, fmt.Sprintf("drive %s %s with no previous reading", s.ID, s.State))
			}
			age := p.Now.Sub(s.LastGood.At)
			if age > p.StaleAfter {
				return Failsafe(p, fmt.Sprintf("drive %s %s, last reading %s old", s.ID, s.State, age.Round(time.Second)))
			}
			stale++
			take(s.LastGood.TempMC)
		default:
			return Failsafe(p, fmt.Sprintf("drive %s in unknown state %q", s.ID, s.State))
		}
	}
	if asleep == len(samples) {
		return Result{ValueMC: p.StandbyMC, Mode: ModeStandby, Reason: fmt.Sprintf("all %d drives asleep", asleep)}
	}
	if !have {
		return Failsafe(p, "no readings")
	}
	return Result{
		ValueMC: maxMC,
		Mode:    ModeNormal,
		Reason:  fmt.Sprintf("max of %d drives (%d fresh, %d last-known, %d asleep)", len(samples), fresh, stale, asleep),
	}
}
