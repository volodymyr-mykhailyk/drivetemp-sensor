// Package daemon runs the discover → read → aggregate → write cycle and owns
// the startup and shutdown failsafe.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/aggregate"
	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/config"
	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/discovery"
	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/output"
	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/power"
	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/reader"
)

// Deps are replaceable dependencies, for tests.
type Deps struct {
	Now    func() time.Time
	Read   reader.ReadFunc
	Logger *slog.Logger
}

type driveSeen struct {
	block string
	state reader.State
}

// Daemon publishes the sensors. It is not safe for concurrent use.
type Daemon struct {
	cfg      *config.Config
	log      *slog.Logger
	now      func() time.Time
	reader   *reader.Reader
	lastGood map[string]aggregate.Reading
	seen     map[string]driveSeen
	modes    map[string]aggregate.Mode
}

// New returns a Daemon for cfg.
func New(cfg *config.Config, deps Deps) *Daemon {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Read == nil {
		var check reader.PowerCheckFunc
		if cfg.Standby == "skip" {
			check = power.Check
		}
		deps.Read = reader.FileReadFunc("/dev", check)
	}
	return &Daemon{
		cfg:      cfg,
		log:      deps.Logger,
		now:      deps.Now,
		reader:   reader.New(deps.Read, cfg.ReadTimeout, deps.Now),
		lastGood: map[string]aggregate.Reading{},
		seen:     map[string]driveSeen{},
		modes:    map[string]aggregate.Mode{},
	}
}

// Run writes the startup failsafe, cycles every interval until ctx is done,
// then applies --on-exit.
func (d *Daemon) Run(ctx context.Context) error {
	if err := d.Start(); err != nil {
		return err
	}
	d.Cycle(ctx)
	t := time.NewTicker(d.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return d.Shutdown()
		case <-t.C:
			d.Cycle(ctx)
		}
	}
}

// Start creates the output directory and writes the failsafe value to every
// enabled sensor, so no consumer sees a value left from an earlier run.
func (d *Daemon) Start() error {
	if err := os.MkdirAll(d.cfg.OutputDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := d.writeFailsafe(); err != nil {
		return fmt.Errorf("write startup failsafe: %w", err)
	}
	d.log.Info("started", "output_dir", d.cfg.OutputDir, "sensors", d.cfg.Sensors,
		"interval", d.cfg.Interval, "standby", d.cfg.Standby, "failsafe_temp", d.cfg.FailsafeTempsC())
	return nil
}

// Shutdown applies --on-exit.
func (d *Daemon) Shutdown() error {
	if d.cfg.OnExit != "failsafe" {
		d.log.Info("stopping, keeping last values")
		return nil
	}
	err := d.writeFailsafe()
	if err != nil {
		d.log.Error("stopping, could not write failsafe value", "err", err)
		return err
	}
	d.log.Info("stopping, wrote failsafe value", "failsafe_temp", d.cfg.FailsafeTempsC())
	return nil
}

func (d *Daemon) writeFailsafe() error {
	var errs []error
	for _, s := range d.cfg.Sensors {
		if err := output.WriteSensor(d.cfg.OutputDir, s, d.cfg.FailsafeMCFor(s)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s, err))
		}
	}
	return errors.Join(errs...)
}

func (d *Daemon) params(sensor string, minDrives int) aggregate.Params {
	return aggregate.Params{
		Now:        d.now(),
		StaleAfter: d.cfg.StaleAfter,
		FailsafeMC: d.cfg.FailsafeMCFor(sensor),
		StandbyMC:  d.cfg.StandbyMC(),
		MinDrives:  minDrives,
	}
}

func sensorClass(name string) discovery.Class {
	if name == config.SensorSSDMax {
		return discovery.SSD
	}
	return discovery.HDD
}

func (d *Daemon) minFor(c discovery.Class) int {
	if c == discovery.SSD {
		return d.cfg.MinSSD
	}
	return d.cfg.MinHDD
}

// Cycle runs one discovery, read, aggregate and write pass and returns the
// value written for each enabled sensor. Any internal error results in the
// failsafe value.
func (d *Daemon) Cycle(ctx context.Context) (results map[string]aggregate.Result) {
	defer func() {
		if r := recover(); r != nil {
			d.log.Error("internal error, writing failsafe", "panic", r)
			results = map[string]aggregate.Result{}
			for _, s := range d.cfg.Sensors {
				results[s] = aggregate.Failsafe(d.params(s, 0), fmt.Sprintf("internal error: %v", r))
			}
			if err := d.writeFailsafe(); err != nil {
				d.log.Error("could not write failsafe value", "err", err)
			}
			d.trackModes(results)
		}
	}()

	results = map[string]aggregate.Result{}
	drives, err := discovery.Discover(discovery.Options{SysfsRoot: d.cfg.SysfsRoot, Exclude: d.cfg.Exclude})
	if err != nil {
		for _, s := range d.cfg.Sensors {
			results[s] = aggregate.Failsafe(d.params(s, 0), "discovery failed: "+err.Error())
		}
		d.writeResults(results)
		d.trackModes(results)
		d.writeStatus(nil, nil, results)
		return results
	}

	active := d.trackDrives(drives)
	reads, late := d.reader.ReadAll(ctx, active)
	for _, l := range late {
		if l.Result.State == reader.OK {
			d.remember(l.ID, l.Result)
		}
	}

	byClass := map[discovery.Class][]aggregate.Sample{}
	for _, drv := range active {
		r := reads[drv.ID]
		s := aggregate.Sample{ID: drv.ID, State: r.State, TempMC: r.TempMC}
		if lg, ok := d.lastGood[drv.ID]; ok {
			s.LastGood = &lg
		}
		byClass[drv.Class] = append(byClass[drv.Class], s)
		d.trackState(drv, r)
	}
	for _, drv := range active {
		if r := reads[drv.ID]; r.State == reader.OK {
			d.remember(drv.ID, r)
		}
	}

	for _, s := range d.cfg.Sensors {
		c := sensorClass(s)
		results[s] = aggregate.Max(byClass[c], d.params(s, d.minFor(c)))
	}
	d.writeResults(results)
	d.trackModes(results)
	d.pruneLastGood()
	d.writeStatus(active, reads, results)
	return results
}

func (d *Daemon) remember(id string, r reader.Result) {
	if lg, ok := d.lastGood[id]; ok && lg.At.After(r.At) {
		return
	}
	d.lastGood[id] = aggregate.Reading{TempMC: r.TempMC, At: r.At}
}

func (d *Daemon) pruneLastGood() {
	now := d.now()
	for id, lg := range d.lastGood {
		if now.Sub(lg.At) > d.cfg.StaleAfter {
			delete(d.lastGood, id)
		}
	}
}

func (d *Daemon) writeResults(results map[string]aggregate.Result) {
	for _, s := range d.cfg.Sensors {
		r := results[s]
		err := output.WriteSensor(d.cfg.OutputDir, s, r.ValueMC)
		if err == nil {
			d.log.Debug("sensor", "sensor", s, "value_mc", r.ValueMC, "mode", r.Mode, "reason", r.Reason)
			continue
		}
		d.log.Error("could not write sensor, trying failsafe", "sensor", s, "err", err)
		results[s] = aggregate.Failsafe(d.params(s, 0), "write failed: "+err.Error())
		if err := output.WriteSensor(d.cfg.OutputDir, s, d.cfg.FailsafeMCFor(s)); err != nil {
			d.log.Error("could not write failsafe value", "sensor", s, "err", err)
		}
	}
}

// trackDrives logs drives that appeared or disappeared and returns the
// drives that take part in aggregation.
func (d *Daemon) trackDrives(drives []discovery.Drive) []discovery.Drive {
	var active []discovery.Drive
	present := map[string]bool{}
	for _, drv := range drives {
		present[drv.ID] = true
		prev, known := d.seen[drv.ID]
		attrs := []any{"drive", drv.ID, "block", drv.Block, "model", drv.Model, "class", drv.Class}
		switch {
		case drv.ExcludedBy != "":
			if !known {
				d.log.Info("drive excluded", append(attrs, "pattern", drv.ExcludedBy)...)
				d.seen[drv.ID] = driveSeen{block: drv.Block}
			}
			continue
		case drv.Class == discovery.Unknown:
			if !known {
				d.log.Warn("drive ignored: queue/rotational is missing or unexpected", attrs...)
				d.seen[drv.ID] = driveSeen{block: drv.Block}
			}
			continue
		case !known:
			d.log.Info("drive appeared", attrs...)
			d.seen[drv.ID] = driveSeen{block: drv.Block}
		case prev.block != drv.Block:
			d.log.Info("drive renamed", append(attrs, "previous_block", prev.block)...)
			prev.block = drv.Block
			d.seen[drv.ID] = prev
		}
		active = append(active, drv)
	}
	for id, s := range d.seen {
		if !present[id] {
			d.log.Info("drive disappeared", "drive", id, "block", s.block)
			delete(d.seen, id)
		}
	}
	return active
}

func (d *Daemon) trackState(drv discovery.Drive, r reader.Result) {
	prev := d.seen[drv.ID]
	if prev.state == r.State {
		return
	}
	attrs := []any{"drive", drv.ID, "block", drv.Block}
	switch r.State {
	case reader.OK:
		if prev.state == "" {
			d.log.Info("drive reading", append(attrs, "temp_mc", r.TempMC)...)
		} else {
			d.log.Info("drive recovered", append(attrs, "temp_mc", r.TempMC, "was", prev.state)...)
		}
	case reader.Failed:
		d.log.Warn("drive read failed", append(attrs, "err", r.Err)...)
	case reader.Pending:
		d.log.Warn("drive read pending", append(attrs, "since", r.At)...)
	case reader.Asleep:
		d.log.Info("drive asleep", attrs...)
	}
	prev.state = r.State
	d.seen[drv.ID] = prev
}

func (d *Daemon) trackModes(results map[string]aggregate.Result) {
	for _, s := range d.cfg.Sensors {
		r := results[s]
		prev, known := d.modes[s]
		if known && prev == r.Mode {
			continue
		}
		attrs := []any{"sensor", s, "value_mc", r.ValueMC, "reason", r.Reason}
		switch {
		case r.Mode == aggregate.ModeFailsafe:
			d.log.Warn("sensor entered failsafe", attrs...)
		case prev == aggregate.ModeFailsafe || !known:
			// Start wrote the failsafe value, so the first result leaves it.
			d.log.Info("sensor left failsafe", append(attrs, "mode", r.Mode)...)
		default:
			d.log.Info("sensor mode changed", append(attrs, "mode", r.Mode)...)
		}
		d.modes[s] = r.Mode
	}
}

// Status is the JSON written to --status-file.
type Status struct {
	Time    time.Time      `json:"time"`
	Drives  []DriveStatus  `json:"drives"`
	Sensors []SensorStatus `json:"sensors"`
}

// DriveStatus is one drive in Status.
type DriveStatus struct {
	ID      string  `json:"id"`
	Block   string  `json:"block"`
	Model   string  `json:"model,omitempty"`
	Serial  string  `json:"serial,omitempty"`
	WWID    string  `json:"wwid,omitempty"`
	Class   string  `json:"class"`
	State   string  `json:"state"`
	TempMC  *int    `json:"temp_mc,omitempty"`
	AgeSecs float64 `json:"age_seconds,omitempty"`
	Error   string  `json:"error,omitempty"`
}

// SensorStatus is one sensor in Status.
type SensorStatus struct {
	Name       string `json:"name"`
	ValueMC    int    `json:"value_mc"`
	Mode       string `json:"mode"`
	Reason     string `json:"reason"`
	FailsafeMC int    `json:"failsafe_mc"`
}

func (d *Daemon) writeStatus(drives []discovery.Drive, reads map[string]reader.Result, results map[string]aggregate.Result) {
	if d.cfg.StatusFile == "" {
		return
	}
	now := d.now()
	st := Status{Time: now, Drives: []DriveStatus{}, Sensors: []SensorStatus{}}
	for _, drv := range drives {
		r := reads[drv.ID]
		ds := DriveStatus{ID: drv.ID, Block: drv.Block, Model: drv.Model, Serial: drv.Serial,
			WWID: drv.WWID, Class: string(drv.Class), State: string(r.State)}
		if r.Err != nil {
			ds.Error = r.Err.Error()
		}
		if lg, ok := d.lastGood[drv.ID]; ok {
			v := lg.TempMC
			ds.TempMC = &v
			ds.AgeSecs = now.Sub(lg.At).Seconds()
		}
		st.Drives = append(st.Drives, ds)
	}
	names := append([]string(nil), d.cfg.Sensors...)
	sort.Strings(names)
	for _, s := range names {
		r := results[s]
		st.Sensors = append(st.Sensors, SensorStatus{Name: s, ValueMC: r.ValueMC, Mode: string(r.Mode), Reason: r.Reason,
			FailsafeMC: d.cfg.FailsafeMCFor(s)})
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		d.log.Warn("could not encode status", "err", err)
		return
	}
	dir, name := filepath.Split(d.cfg.StatusFile)
	if dir == "" {
		dir = "."
	}
	if err := output.WriteAtomic(dir, name, append(b, '\n')); err != nil {
		d.log.Warn("could not write status file", "err", err)
	}
}
