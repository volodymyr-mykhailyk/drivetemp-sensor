package discovery

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/volodymyr-mykhailyk/drivetemp-sensor/internal/testutil"
)

func byBlock(drives []Drive) map[string]Drive {
	m := map[string]Drive{}
	for _, d := range drives {
		m[d.Block] = d
	}
	return m
}

func TestDiscoverFindsOnlyDrivetempDevices(t *testing.T) {
	root := testutil.SysfsDir(t)
	drives, err := Discover(Options{SysfsRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	var blocks []string
	for _, d := range drives {
		blocks = append(blocks, d.Block)
	}
	// sde (no hwmon), sdf (hwmon not named drivetemp), loop0, md1 and dm-0
	// (no device) are skipped.
	want := []string{"sda", "sdb", "sdc", "sdd", "sdg"}
	if len(blocks) != len(want) {
		t.Fatalf("blocks = %v, want %v", blocks, want)
	}
	for i := range want {
		if blocks[i] != want[i] {
			t.Fatalf("blocks = %v, want %v", blocks, want)
		}
	}
}

func TestDiscoverClassIdentityAndPaths(t *testing.T) {
	root := testutil.SysfsDir(t)
	drives, err := Discover(Options{SysfsRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	m := byBlock(drives)
	tests := []struct {
		block, id, model, serial string
		class                    Class
		hwmon                    string
	}{
		{"sda", "naa.5000c500a0000001", "ST6000VN001-2BB186", "ZR10000A", HDD, "hwmon2"},
		{"sdb", "Z840000B", "ST8000AS0002-1NA17Z", "Z840000B", HDD, "hwmon3"},
		{"sdc", "naa.500a0751a0000003", "CT500MX500SSD1", "2000E0000003", SSD, "hwmon5"},
		{"sdd", "WXA1A0000004", "Elements 25A3", "WXA1A0000004", HDD, "hwmon6"},
		{"sdg", "Generic SSD@sdg", "Generic SSD", "", SSD, "hwmon8"},
	}
	for _, tt := range tests {
		d, ok := m[tt.block]
		if !ok {
			t.Errorf("%s not discovered", tt.block)
			continue
		}
		if d.ID != tt.id || d.Model != tt.model || d.Serial != tt.serial || d.Class != tt.class {
			t.Errorf("%s = {ID:%q Model:%q Serial:%q Class:%q}, want {%q %q %q %q}",
				tt.block, d.ID, d.Model, d.Serial, d.Class, tt.id, tt.model, tt.serial, tt.class)
		}
		wantPath := filepath.Join(root, "block", tt.block, "device", "hwmon", tt.hwmon, "temp1_input")
		if d.TempPath != wantPath {
			t.Errorf("%s TempPath = %s, want %s", tt.block, d.TempPath, wantPath)
		}
		if d.ExcludedBy != "" {
			t.Errorf("%s excluded without patterns", tt.block)
		}
	}
}

func TestIdentitySurvivesRenumbering(t *testing.T) {
	root := testutil.CopySysfs(t)
	before, err := Discover(Options{SysfsRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	// Kernel upgrade / re-plug: sda becomes sdh and its hwmon number changes.
	dev := filepath.Join(root, "block", "sda")
	if err := os.Rename(filepath.Join(dev, "device", "hwmon", "hwmon2"), filepath.Join(dev, "device", "hwmon", "hwmon12")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dev, filepath.Join(root, "block", "sdh")); err != nil {
		t.Fatal(err)
	}
	after, err := Discover(Options{SysfsRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := byBlock(after)["sdh"].ID, byBlock(before)["sda"].ID; got != want {
		t.Errorf("ID after rename = %q, want %q", got, want)
	}
	// Drive without WWID and serial: identity follows the block name.
	if got := byBlock(after)["sdg"].ID; got != "Generic SSD@sdg" {
		t.Errorf("sdg ID = %q", got)
	}
}

func TestDiscoverPicksUpAddedAndRemovedDrives(t *testing.T) {
	root := testutil.CopySysfs(t)
	if err := os.RemoveAll(filepath.Join(root, "block", "sdb")); err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, root, "block/sdi/queue/rotational", "1\n")
	testutil.WriteFile(t, root, "block/sdi/device/wwid", "naa.5000c500a0000009\n")
	testutil.WriteFile(t, root, "block/sdi/device/hwmon/hwmon9/name", "drivetemp\n")
	testutil.WriteFile(t, root, "block/sdi/device/hwmon/hwmon9/temp1_input", "40000\n")
	drives, err := Discover(Options{SysfsRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	m := byBlock(drives)
	if _, ok := m["sdb"]; ok {
		t.Error("removed sdb still discovered")
	}
	if d, ok := m["sdi"]; !ok || d.Class != HDD || d.ID != "naa.5000c500a0000009" {
		t.Errorf("added sdi = %+v, ok=%v", d, ok)
	}
}

func TestUnknownClass(t *testing.T) {
	root := testutil.CopySysfs(t)
	if err := os.Remove(filepath.Join(root, "block", "sda", "queue", "rotational")); err != nil {
		t.Fatal(err)
	}
	drives, err := Discover(Options{SysfsRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if c := byBlock(drives)["sda"].Class; c != Unknown {
		t.Errorf("class = %q, want unknown", c)
	}
}

func TestExclude(t *testing.T) {
	root := testutil.SysfsDir(t)
	tests := []struct {
		name     string
		patterns []string
		excluded map[string]string
	}{
		{"by model", []string{"Elements*"}, map[string]string{"sdd": "Elements*"}},
		{"by serial", []string{"Z840000?"}, map[string]string{"sdb": "Z840000?"}},
		{"by wwid", []string{"naa.500a0751*"}, map[string]string{"sdc": "naa.500a0751*"}},
		{"first match wins", []string{"nomatch", "ST*"}, map[string]string{"sda": "ST*", "sdb": "ST*"}},
		{"no match", []string{"XYZ*"}, map[string]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drives, err := Discover(Options{SysfsRoot: root, Exclude: tt.patterns})
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range drives {
				if d.ExcludedBy != tt.excluded[d.Block] {
					t.Errorf("%s ExcludedBy = %q, want %q", d.Block, d.ExcludedBy, tt.excluded[d.Block])
				}
			}
		})
	}
}

func TestValidatePatterns(t *testing.T) {
	if err := ValidatePatterns([]string{"ok*", "a?b"}); err != nil {
		t.Errorf("valid patterns rejected: %v", err)
	}
	if err := ValidatePatterns([]string{"[bad"}); err == nil {
		t.Error("malformed pattern accepted")
	}
}

func TestDiscoverMissingRoot(t *testing.T) {
	if _, err := Discover(Options{SysfsRoot: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Error("expected error for missing sysfs")
	}
}

func TestParseVPDSerial(t *testing.T) {
	tests := []struct {
		in   []byte
		want string
		err  bool
	}{
		{[]byte{0, 0x80, 0, 4, 'A', 'B', 'C', 'D'}, "ABCD", false},
		{[]byte{0, 0x80, 0, 8, ' ', ' ', 'X', 'Y', ' ', 0, 0, 0}, "XY", false},
		{[]byte{0, 0x80, 0, 20, 'S', 'N'}, "SN", false}, // length beyond data
		{[]byte{0, 0x83, 0, 2, 'A', 'B'}, "", true},
		{[]byte{0, 0x80}, "", true},
	}
	for _, tt := range tests {
		got, err := parseVPDSerial(tt.in)
		if (err != nil) != tt.err || got != tt.want {
			t.Errorf("parseVPDSerial(%v) = %q, %v; want %q, err=%v", tt.in, got, err, tt.want, tt.err)
		}
	}
}
