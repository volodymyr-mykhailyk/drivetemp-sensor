package aggregate

import (
	"strings"
	"testing"
	"time"

	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/reader"
)

var now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func params() Params {
	return Params{Now: now, StaleAfter: 60 * time.Second, FailsafeMC: 100000, StandbyMC: 30000, MinDrives: 1}
}

func ok(id string, mc int) Sample { return Sample{ID: id, State: reader.OK, TempMC: mc} }

func last(id string, st reader.State, mc int, age time.Duration) Sample {
	return Sample{ID: id, State: st, LastGood: &Reading{TempMC: mc, At: now.Add(-age)}}
}

func asleep(id string) Sample { return Sample{ID: id, State: reader.Asleep} }

// TestMaxAggregationTable covers every row of the specification's
// aggregation table (WB-003 "Aggregation and output values").
func TestMaxAggregationTable(t *testing.T) {
	tests := []struct {
		name    string
		row     string
		samples []Sample
		min     int
		want    int
		mode    Mode
		reason  string
	}{
		// Row 1: at least one drive with a fresh reading → max.
		{name: "fresh readings: max", row: "1",
			samples: []Sample{ok("a", 38000), ok("b", 41000), ok("c", 39000)}, want: 41000, mode: ModeNormal},
		{name: "single fresh reading", row: "1",
			samples: []Sample{ok("a", 38000)}, want: 38000, mode: ModeNormal},
		{name: "fresh max ignores an older lower last-known reading", row: "1",
			samples: []Sample{ok("a", 45000), last("b", reader.Failed, 40000, 30*time.Second)}, want: 45000, mode: ModeNormal},
		// Row 2: failed or pending, last good younger than stale-after → takes part.
		{name: "failed drive's recent reading takes part in max", row: "2",
			samples: []Sample{ok("a", 38000), last("b", reader.Failed, 47000, 30*time.Second)}, want: 47000, mode: ModeNormal},
		{name: "pending drive's recent reading takes part in max", row: "2",
			samples: []Sample{ok("a", 38000), last("b", reader.Pending, 46000, 59*time.Second)}, want: 46000, mode: ModeNormal},
		{name: "reading exactly stale-after old still counts", row: "2",
			samples: []Sample{last("a", reader.Failed, 42000, 60*time.Second)}, want: 42000, mode: ModeNormal},
		{name: "all drives failed but recent", row: "2",
			samples: []Sample{last("a", reader.Failed, 42000, 10*time.Second), last("b", reader.Pending, 43000, 10*time.Second)}, want: 43000, mode: ModeNormal},
		// Row 3: failed or pending longer than stale-after → failsafe.
		{name: "failed drive past stale-after", row: "3",
			samples: []Sample{ok("a", 38000), last("b", reader.Failed, 30000, 61*time.Second)}, want: 100000, mode: ModeFailsafe, reason: "drive b failed"},
		{name: "pending drive past stale-after", row: "3",
			samples: []Sample{ok("a", 38000), last("b", reader.Pending, 30000, 2*time.Minute)}, want: 100000, mode: ModeFailsafe, reason: "drive b pending"},
		{name: "failed drive never read", row: "3",
			samples: []Sample{ok("a", 38000), {ID: "b", State: reader.Failed}}, want: 100000, mode: ModeFailsafe, reason: "no previous reading"},
		{name: "pending drive never read", row: "3",
			samples: []Sample{{ID: "b", State: reader.Pending}}, want: 100000, mode: ModeFailsafe, reason: "no previous reading"},
		// Row 4: fewer drives than --min-hdd/--min-ssd → failsafe.
		{name: "fewer drives than minimum", row: "4",
			samples: []Sample{ok("a", 38000), ok("b", 39000)}, min: 3, want: 100000, mode: ModeFailsafe, reason: "need at least 3"},
		{name: "exactly the minimum", row: "4",
			samples: []Sample{ok("a", 38000), ok("b", 39000), ok("c", 40000)}, min: 3, want: 40000, mode: ModeNormal},
		{name: "no drives with default minimum", row: "4",
			samples: nil, min: 1, want: 100000, mode: ModeFailsafe, reason: "no drives"},
		{name: "no drives even with minimum 0", row: "4",
			samples: nil, min: 0, want: 100000, mode: ModeFailsafe, reason: "no drives"},
		// Row 5: all drives asleep → standby temperature.
		{name: "all asleep", row: "5",
			samples: []Sample{asleep("a"), asleep("b")}, want: 30000, mode: ModeStandby},
		{name: "some asleep: max of awake drives", row: "5",
			samples: []Sample{asleep("a"), ok("b", 36000)}, want: 36000, mode: ModeNormal},
		{name: "asleep and recently failed", row: "5",
			samples: []Sample{asleep("a"), last("b", reader.Failed, 37000, 5*time.Second)}, want: 37000, mode: ModeNormal},
		{name: "asleep and stale failed", row: "5",
			samples: []Sample{asleep("a"), last("b", reader.Failed, 37000, 5*time.Minute)}, want: 100000, mode: ModeFailsafe},
		// Row 6: internal error → failsafe.
		{name: "unknown drive state", row: "6",
			samples: []Sample{ok("a", 38000), {ID: "b", State: "bogus"}}, want: 100000, mode: ModeFailsafe, reason: "unknown state"},
	}
	for _, tt := range tests {
		t.Run("row "+tt.row+"/"+tt.name, func(t *testing.T) {
			p := params()
			if tt.min != 0 || tt.samples == nil {
				p.MinDrives = tt.min
			}
			got := Max(tt.samples, p)
			if got.ValueMC != tt.want || got.Mode != tt.mode {
				t.Errorf("Max = %d %s (%s), want %d %s", got.ValueMC, got.Mode, got.Reason, tt.want, tt.mode)
			}
			if tt.reason != "" && !strings.Contains(got.Reason, tt.reason) {
				t.Errorf("reason %q does not contain %q", got.Reason, tt.reason)
			}
			if got.Reason == "" {
				t.Error("empty reason")
			}
		})
	}
}

func TestFailsafe(t *testing.T) {
	r := Failsafe(params(), "x")
	if r.ValueMC != 100000 || r.Mode != ModeFailsafe || r.Reason != "x" {
		t.Errorf("Failsafe = %+v", r)
	}
}
