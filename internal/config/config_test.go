package config

import (
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	c, err := Parse("t", nil, envFrom(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		OutputDir: "/run/drivetemp-sensor", Sensors: []string{"hdd-max", "ssd-max"},
		Interval: 10 * time.Second, ReadTimeout: 5 * time.Second, StaleAfter: 60 * time.Second,
		FailsafeTempC: 100, Standby: "read", StandbyTempC: 30, MinHDD: 1, MinSSD: 1,
		OnExit: "failsafe", SysfsRoot: "/sys", LogLevel: slog.LevelInfo, logLevelString: "info",
	}
	if !reflect.DeepEqual(*c, want) {
		t.Errorf("defaults =\n%+v\nwant\n%+v", *c, want)
	}
	if c.FailsafeMC() != 100000 || c.StandbyMC() != 30000 {
		t.Errorf("millidegrees: %d %d", c.FailsafeMC(), c.StandbyMC())
	}
}

func TestEnvAndFlags(t *testing.T) {
	env := envFrom(map[string]string{
		"DRIVETEMP_SENSOR_INTERVAL":      "20s",
		"DRIVETEMP_SENSOR_FAILSAFE_TEMP": "90",
		"DRIVETEMP_SENSOR_SENSORS":       "hdd-max",
		"DRIVETEMP_SENSOR_EXCLUDE":       "WD*, Elements*",
		"DRIVETEMP_SENSOR_MIN_HDD":       "3",
		"DRIVETEMP_SENSOR_LOG_LEVEL":     "debug",
	})
	c, err := Parse("t", []string{"--failsafe-temp", "95", "--exclude", "naa.5*"}, env, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Interval != 20*time.Second {
		t.Errorf("interval from env = %s", c.Interval)
	}
	if c.FailsafeTempC != 95 {
		t.Errorf("flag should override env: failsafe = %d", c.FailsafeTempC)
	}
	if !reflect.DeepEqual(c.Sensors, []string{"hdd-max"}) || !c.SensorEnabled("hdd-max") || c.SensorEnabled("ssd-max") {
		t.Errorf("sensors = %v", c.Sensors)
	}
	if !reflect.DeepEqual(c.Exclude, []string{"WD*", "Elements*", "naa.5*"}) {
		t.Errorf("exclude = %v", c.Exclude)
	}
	if c.MinHDD != 3 || c.LogLevel != slog.LevelDebug {
		t.Errorf("min-hdd = %d, log level = %s", c.MinHDD, c.LogLevel)
	}
}

func TestEnvName(t *testing.T) {
	if got := EnvName("read-timeout"); got != "DRIVETEMP_SENSOR_READ_TIMEOUT" {
		t.Errorf("EnvName = %s", got)
	}
}

func TestInvalid(t *testing.T) {
	tests := []struct {
		args []string
		env  map[string]string
		want string
	}{
		{args: []string{"--sensors", "hdd-avg"}, want: "unknown sensor"},
		{args: []string{"--sensors", ""}, want: "at least one sensor"},
		{args: []string{"--sensors", "hdd-max,hdd-max"}, want: "listed twice"},
		{args: []string{"--interval", "0s"}, want: "--interval"},
		{args: []string{"--read-timeout", "20s"}, want: "--read-timeout"},
		{args: []string{"--stale-after", "-1s"}, want: "--stale-after"},
		{args: []string{"--failsafe-temp", "200"}, want: "--failsafe-temp"},
		{args: []string{"--standby-temp", "-5"}, want: "--standby-temp"},
		{args: []string{"--standby", "sometimes"}, want: "--standby"},
		{args: []string{"--on-exit", "explode"}, want: "--on-exit"},
		{args: []string{"--min-ssd", "-1"}, want: "--min-hdd"},
		{args: []string{"--exclude", "[bad"}, want: "--exclude"},
		{args: []string{"--log-level", "loud"}, want: "--log-level"},
		{args: []string{"--output-dir", ""}, want: "--output-dir"},
		{args: []string{"extra"}, want: "unexpected arguments"},
		{args: []string{"--interval", "fast"}, want: "invalid value"},
		{env: map[string]string{"DRIVETEMP_SENSOR_INTERVAL": "fast"}, want: "DRIVETEMP_SENSOR_INTERVAL"},
	}
	for _, tt := range tests {
		_, err := Parse("t", tt.args, envFrom(tt.env), io.Discard)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Parse(%v, %v) err = %v, want containing %q", tt.args, tt.env, err, tt.want)
		}
	}
}

func TestUsageMentionsEnv(t *testing.T) {
	var b strings.Builder
	_, err := Parse("drivetemp-sensor", []string{"--help"}, envFrom(nil), &b)
	if err == nil {
		t.Fatal("expected flag.ErrHelp")
	}
	if !strings.Contains(b.String(), "DRIVETEMP_SENSOR_INTERVAL") || !strings.Contains(b.String(), "healthcheck") {
		t.Errorf("usage:\n%s", b.String())
	}
}
