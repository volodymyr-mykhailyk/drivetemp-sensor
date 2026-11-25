// Package discovery finds drives that expose a kernel drivetemp hwmon sensor
// and classifies them as HDD or SSD.
//
// Discovery is stateless: every call lists sysfs from scratch, so drives that
// are added, removed or renamed are picked up without configuration.
package discovery

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Class is the drive class a sensor aggregates over.
type Class string

const (
	HDD     Class = "hdd"
	SSD     Class = "ssd"
	Unknown Class = "unknown"
)

// Drive is one block device with a drivetemp sensor.
type Drive struct {
	// ID is the stable identity: WWID, else serial, else model and block name.
	ID string
	// Block is the kernel block device name, e.g. "sda". Not stable.
	Block    string
	Model    string
	Serial   string
	WWID     string
	Class    Class
	TempPath string
	// ExcludedBy is the --exclude pattern that matched, empty if included.
	ExcludedBy string
}

// Options controls discovery.
type Options struct {
	// SysfsRoot is the sysfs mount point, normally "/sys".
	SysfsRoot string
	// Exclude holds glob patterns (path.Match syntax) matched against model,
	// serial and WWID.
	Exclude []string
}

// ValidatePatterns reports the first malformed exclude pattern.
func ValidatePatterns(patterns []string) error {
	for _, p := range patterns {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("invalid exclude pattern %q: %w", p, err)
		}
	}
	return nil
}

// Discover lists all drives with a drivetemp sensor, sorted by block name.
// Excluded drives are returned with ExcludedBy set so callers can log them.
func Discover(opts Options) ([]Drive, error) {
	blockDir := filepath.Join(opts.SysfsRoot, "block")
	entries, err := os.ReadDir(blockDir)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", blockDir, err)
	}
	var drives []Drive
	for _, e := range entries {
		name := e.Name()
		dev := filepath.Join(blockDir, name)
		temp, ok := findDrivetemp(filepath.Join(dev, "device"))
		if !ok {
			continue
		}
		d := Drive{
			Block:    name,
			TempPath: temp,
			Model:    readTrim(filepath.Join(dev, "device", "model")),
			WWID:     readTrim(filepath.Join(dev, "device", "wwid")),
			Serial:   readVPDSerial(filepath.Join(dev, "device", "vpd_pg80")),
			Class:    readClass(filepath.Join(dev, "queue", "rotational")),
		}
		d.ID = identity(d)
		d.ExcludedBy = excludedBy(d, opts.Exclude)
		drives = append(drives, d)
	}
	sort.Slice(drives, func(i, j int) bool { return drives[i].Block < drives[j].Block })
	return drives, nil
}

// findDrivetemp returns the temp1_input path of the first hwmon device under
// devDir whose name is "drivetemp".
func findDrivetemp(devDir string) (string, bool) {
	hwmons, err := filepath.Glob(filepath.Join(devDir, "hwmon", "hwmon*"))
	if err != nil {
		return "", false
	}
	sort.Strings(hwmons)
	for _, h := range hwmons {
		if readTrim(filepath.Join(h, "name")) == "drivetemp" {
			return filepath.Join(h, "temp1_input"), true
		}
	}
	return "", false
}

func readClass(p string) Class {
	switch readTrim(p) {
	case "1":
		return HDD
	case "0":
		return SSD
	default:
		return Unknown
	}
}

func identity(d Drive) string {
	switch {
	case d.WWID != "":
		return d.WWID
	case d.Serial != "":
		return d.Serial
	default:
		return d.Model + "@" + d.Block
	}
}

func excludedBy(d Drive, patterns []string) string {
	for _, p := range patterns {
		for _, v := range []string{d.Model, d.Serial, d.WWID} {
			if v == "" {
				continue
			}
			if ok, _ := path.Match(p, v); ok {
				return p
			}
		}
	}
	return ""
}

func readTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readVPDSerial parses the unit serial number from a raw SCSI VPD page 0x80.
func readVPDSerial(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	s, err := parseVPDSerial(b)
	if err != nil {
		return ""
	}
	return s
}

func parseVPDSerial(b []byte) (string, error) {
	if len(b) < 4 || b[1] != 0x80 {
		return "", errors.New("not a VPD page 0x80")
	}
	n := int(b[2])<<8 | int(b[3])
	if 4+n > len(b) {
		n = len(b) - 4
	}
	s := bytes.TrimRight(b[4:4+n], "\x00")
	return strings.TrimSpace(string(s)), nil
}
