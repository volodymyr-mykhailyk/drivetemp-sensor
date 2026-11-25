package power

import "testing"

func TestCheckPowerModeCDB(t *testing.T) {
	cdb := checkPowerModeCDB()
	want := [16]byte{0x85, 0x06, 0x20, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xe5, 0}
	if cdb != want {
		t.Errorf("cdb = % x, want % x", cdb, want)
	}
}

// descSense builds descriptor-format sense data with an ATA Status Return
// descriptor, as libata returns for ATA PASS-THROUGH with CK_COND.
func descSense(status, count byte) []byte {
	sb := make([]byte, 22)
	sb[0] = 0x72
	sb[1] = 0x01 // RECOVERED ERROR
	sb[2] = 0x00 // ASC
	sb[3] = 0x1d // ASCQ: ATA pass-through information available
	sb[7] = 14
	d := sb[8:]
	d[0] = 0x09
	d[1] = 0x0c
	d[5] = count
	d[13] = status
	return sb
}

func fixedSense(status, count byte) []byte {
	sb := make([]byte, 18)
	sb[0] = 0x70
	sb[2] = 0x01
	sb[4] = status
	sb[6] = count
	sb[7] = 10
	return sb
}

func TestDecode(t *testing.T) {
	tests := []struct {
		name string
		sb   []byte
		want State
		err  bool
	}{
		{"descriptor standby", descSense(0x50, 0x00), StateStandby, false},
		{"descriptor standby_y", descSense(0x50, 0x01), StateStandby, false},
		{"descriptor NV cache spun down", descSense(0x50, 0x40), StateStandby, false},
		{"descriptor idle", descSense(0x50, 0x80), StateActive, false},
		{"descriptor idle_a", descSense(0x50, 0x81), StateActive, false},
		{"descriptor active", descSense(0x50, 0xff), StateActive, false},
		{"fixed standby", fixedSense(0x50, 0x00), StateStandby, false},
		{"fixed active", fixedSense(0x50, 0xff), StateActive, false},
		{"ATA error bit", descSense(0x51, 0x00), StateUnknown, true},
		{"unknown mode", descSense(0x50, 0x33), StateUnknown, true},
		{"too short", []byte{0x72, 0}, StateUnknown, true},
		{"unknown response code", append([]byte{0x7f}, make([]byte, 20)...), StateUnknown, true},
		{"descriptor without ATA status", func() []byte {
			sb := descSense(0x50, 0)
			sb[8] = 0x02
			return sb
		}(), StateUnknown, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decode(tt.sb)
			if got != tt.want || (err != nil) != tt.err {
				t.Errorf("decode = %v, %v; want %v, err=%v", got, err, tt.want, tt.err)
			}
		})
	}
}

func TestStateString(t *testing.T) {
	for s, want := range map[State]string{StateActive: "active", StateStandby: "standby", StateUnknown: "unknown"} {
		if s.String() != want {
			t.Errorf("%d.String() = %q", s, s.String())
		}
	}
}

func TestCheckMissingDevice(t *testing.T) {
	if _, err := Check("/nonexistent/device/sdz"); err == nil {
		t.Error("expected error")
	}
}
