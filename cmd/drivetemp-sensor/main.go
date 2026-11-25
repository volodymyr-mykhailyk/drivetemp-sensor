// Command drivetemp-sensor publishes the hottest HDD and SSD temperature,
// read from the kernel drivetemp hwmon driver, as plain sensor files for fan
// controllers such as CoolerControl, fan2go and afancontrol.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/config"
	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/daemon"
	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/output"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cmd := "run"
	if len(args) > 0 {
		switch args[0] {
		case "healthcheck", "version":
			cmd, args = args[0], args[1:]
		case "--version", "-version":
			cmd = "version"
		}
	}
	if cmd == "version" {
		fmt.Fprintln(stdout, "drivetemp-sensor", version)
		return 0
	}

	cfg, err := config.Parse("drivetemp-sensor", args, getenv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "drivetemp-sensor:", err)
		return 2
	}

	if cmd == "healthcheck" {
		if err := output.Check(cfg.OutputDir, cfg.Sensors, 3*cfg.Interval, time.Now()); err != nil {
			fmt.Fprintln(stderr, "unhealthy:", err)
			return 1
		}
		return 0
	}

	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	log.Info("drivetemp-sensor", "version", version)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := daemon.New(cfg, daemon.Deps{Logger: log}).Run(ctx); err != nil {
		log.Error("exiting", "err", err)
		return 1
	}
	return 0
}
