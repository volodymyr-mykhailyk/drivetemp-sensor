// Package config parses command-line flags and their environment variables.
//
// Every flag --some-flag can also be set with DRIVETEMP_SENSOR_SOME_FLAG.
// A flag given on the command line overrides its environment variable;
// --exclude values from both are combined (the variable is comma-separated).
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/discovery"
)

// EnvPrefix is prepended to the upper-snake-case flag name.
const EnvPrefix = "DRIVETEMP_SENSOR_"

// Sensor names.
const (
	SensorHDDMax = "hdd-max"
	SensorSSDMax = "ssd-max"
)

// AllSensors lists every known sensor in output order.
var AllSensors = []string{SensorHDDMax, SensorSSDMax}

// Config is the parsed configuration.
type Config struct {
	OutputDir      string
	Sensors        []string
	Interval       time.Duration
	ReadTimeout    time.Duration
	StaleAfter     time.Duration
	FailsafeTempC  int
	Standby        string
	StandbyTempC   int
	MinHDD         int
	MinSSD         int
	Exclude        []string
	OnExit         string
	StatusFile     string
	SysfsRoot      string
	LogLevel       slog.Level
	logLevelString string
}

// FailsafeMC returns the failsafe temperature in millidegrees.
func (c Config) FailsafeMC() int { return c.FailsafeTempC * 1000 }

// StandbyMC returns the standby temperature in millidegrees.
func (c Config) StandbyMC() int { return c.StandbyTempC * 1000 }

// SensorEnabled reports whether a sensor is enabled.
func (c Config) SensorEnabled(name string) bool {
	for _, s := range c.Sensors {
		if s == name {
			return true
		}
	}
	return false
}

type listFlag struct{ v *[]string }

func (l listFlag) String() string {
	if l.v == nil {
		return ""
	}
	return strings.Join(*l.v, ",")
}

func (l listFlag) Set(s string) error {
	*l.v = append(*l.v, s)
	return nil
}

// EnvName returns the environment variable for a flag name.
func EnvName(flagName string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// Parse parses args (without the program name) using getenv for environment
// variables. Usage and errors go to out.
func Parse(name string, args []string, getenv func(string) string, out io.Writer) (*Config, error) {
	c := &Config{}
	var sensors string
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&c.OutputDir, "output-dir", "/run/drivetemp-sensor", "directory for the sensor files")
	fs.StringVar(&sensors, "sensors", strings.Join(AllSensors, ","), "comma-separated sensors to publish: hdd-max, ssd-max")
	fs.DurationVar(&c.Interval, "interval", 10*time.Second, "time between cycles")
	fs.DurationVar(&c.ReadTimeout, "read-timeout", 5*time.Second, "how long to wait for a drive read before it counts as pending")
	fs.DurationVar(&c.StaleAfter, "stale-after", 60*time.Second, "how long a drive's last good reading stands in for a failed or pending read")
	fs.IntVar(&c.FailsafeTempC, "failsafe-temp", 100, "value (°C) written when the temperature is unknown")
	fs.StringVar(&c.Standby, "standby", "read", "standby handling: read (read every drive) or skip (do not read drives in standby; experimental)")
	fs.IntVar(&c.StandbyTempC, "standby-temp", 30, "value (°C) written when all drives of a class are asleep (--standby=skip)")
	fs.IntVar(&c.MinHDD, "min-hdd", 1, "fail safe when fewer HDDs are found")
	fs.IntVar(&c.MinSSD, "min-ssd", 1, "fail safe when fewer SSDs are found")
	fs.Var(listFlag{&c.Exclude}, "exclude", "glob matched against model, serial and WWID of drives to ignore (repeatable)")
	fs.StringVar(&c.OnExit, "on-exit", "failsafe", "on SIGTERM/SIGINT: failsafe (write the failsafe value) or keep")
	fs.StringVar(&c.StatusFile, "status-file", "", "write a JSON status file to this path (off when empty)")
	fs.StringVar(&c.SysfsRoot, "sysfs-root", "/sys", "sysfs mount point")
	fs.StringVar(&c.logLevelString, "log-level", "info", "debug, info, warn or error")
	fs.Usage = func() {
		fmt.Fprintf(out, "Usage: %s [flags]\n       %s healthcheck [flags]\n       %s version\n\n", name, name, name)
		fmt.Fprintf(out, "Every flag can be set with %s<FLAG>, e.g. %s.\n\nFlags:\n", EnvPrefix, EnvName("interval"))
		fs.PrintDefaults()
	}

	var envErr error
	fs.VisitAll(func(f *flag.Flag) {
		v := getenv(EnvName(f.Name))
		if v == "" {
			return
		}
		if f.Name == "exclude" {
			for _, p := range strings.Split(v, ",") {
				if p = strings.TrimSpace(p); p != "" {
					c.Exclude = append(c.Exclude, p)
				}
			}
			return
		}
		if err := fs.Set(f.Name, v); err != nil {
			envErr = errors.Join(envErr, fmt.Errorf("%s=%q: %w", EnvName(f.Name), v, err))
		}
	})
	if envErr != nil {
		return nil, envErr
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}

	for _, s := range strings.Split(sensors, ",") {
		if s = strings.TrimSpace(s); s != "" {
			c.Sensors = append(c.Sensors, s)
		}
	}
	return c, c.validate()
}

func (c *Config) validate() error {
	var errs []error
	if len(c.Sensors) == 0 {
		errs = append(errs, errors.New("--sensors: at least one sensor is required"))
	}
	seen := map[string]bool{}
	for _, s := range c.Sensors {
		if s != SensorHDDMax && s != SensorSSDMax {
			errs = append(errs, fmt.Errorf("--sensors: unknown sensor %q (known: %s)", s, strings.Join(AllSensors, ", ")))
		}
		if seen[s] {
			errs = append(errs, fmt.Errorf("--sensors: %q listed twice", s))
		}
		seen[s] = true
	}
	if c.OutputDir == "" {
		errs = append(errs, errors.New("--output-dir must not be empty"))
	}
	if c.Interval <= 0 {
		errs = append(errs, errors.New("--interval must be positive"))
	}
	if c.ReadTimeout <= 0 || c.ReadTimeout > c.Interval {
		errs = append(errs, errors.New("--read-timeout must be positive and not longer than --interval"))
	}
	if c.StaleAfter <= 0 {
		errs = append(errs, errors.New("--stale-after must be positive"))
	}
	if c.FailsafeTempC < 0 || c.FailsafeTempC > 150 {
		errs = append(errs, errors.New("--failsafe-temp must be between 0 and 150 °C"))
	}
	if c.StandbyTempC < 0 || c.StandbyTempC > 150 {
		errs = append(errs, errors.New("--standby-temp must be between 0 and 150 °C"))
	}
	if c.Standby != "read" && c.Standby != "skip" {
		errs = append(errs, fmt.Errorf("--standby must be read or skip, not %q", c.Standby))
	}
	if c.OnExit != "failsafe" && c.OnExit != "keep" {
		errs = append(errs, fmt.Errorf("--on-exit must be failsafe or keep, not %q", c.OnExit))
	}
	if c.MinHDD < 0 || c.MinSSD < 0 {
		errs = append(errs, errors.New("--min-hdd and --min-ssd must not be negative"))
	}
	if err := discovery.ValidatePatterns(c.Exclude); err != nil {
		errs = append(errs, fmt.Errorf("--exclude: %w", err))
	}
	if err := c.LogLevel.UnmarshalText([]byte(c.logLevelString)); err != nil {
		errs = append(errs, fmt.Errorf("--log-level: %w", err))
	}
	return errors.Join(errs...)
}
