// Package power asks an ATA drive for its power state with CHECK POWER MODE
// (the command `hdparm -C` sends), which does not spin the drive up.
//
// The command is sent through the SCSI generic SG_IO ioctl as an ATA
// PASS-THROUGH (16) CDB. That needs the block device node and CAP_SYS_RAWIO.
package power

import (
	"errors"
	"fmt"
)

// State is a drive power state.
type State int

const (
	StateUnknown State = iota
	// StateActive covers active and all idle states: the platters spin.
	StateActive
	// StateStandby covers standby and spun-down NV-cache states.
	StateStandby
)

func (s State) String() string {
	switch s {
	case StateActive:
		return "active"
	case StateStandby:
		return "standby"
	default:
		return "unknown"
	}
}

// ErrUnsupported is returned on platforms without SG_IO.
var ErrUnsupported = errors.New("ATA power check is only supported on Linux")

const (
	ataCheckPowerMode = 0xe5
	ataPassThrough16  = 0x85
	ataStatusErr      = 0x01
)

// checkPowerModeCDB builds the ATA PASS-THROUGH (16) CDB for CHECK POWER
// MODE: non-data protocol with CK_COND set, so the device returns the ATA
// registers (including COUNT, which holds the power mode) as sense data.
func checkPowerModeCDB() [16]byte {
	var cdb [16]byte
	cdb[0] = ataPassThrough16
	cdb[1] = 3 << 1 // protocol 3: non-data
	cdb[2] = 0x20   // CK_COND
	cdb[14] = ataCheckPowerMode
	return cdb
}

// parseSense extracts ATA STATUS and COUNT(7:0) from the sense data returned
// for an ATA PASS-THROUGH command with CK_COND, in descriptor (0x72/0x73) or
// fixed (0x70/0x71) format.
func parseSense(sb []byte) (status, count byte, err error) {
	if len(sb) < 8 {
		return 0, 0, fmt.Errorf("sense data too short (%d bytes)", len(sb))
	}
	switch sb[0] & 0x7f {
	case 0x72, 0x73:
		addl := int(sb[7])
		end := 8 + addl
		if end > len(sb) {
			end = len(sb)
		}
		for i := 8; i+1 < end; {
			code, l := sb[i], int(sb[i+1])
			if code == 0x09 && l >= 0x0c && i+13 < len(sb) {
				d := sb[i:]
				return d[13], d[5], nil
			}
			i += 2 + l
		}
		return 0, 0, errors.New("no ATA status return descriptor in sense data")
	case 0x70, 0x71:
		if len(sb) < 7 {
			return 0, 0, errors.New("fixed-format sense data too short")
		}
		// SAT: INFORMATION field = ERROR, STATUS, DEVICE, COUNT(7:0).
		return sb[4], sb[6], nil
	default:
		return 0, 0, fmt.Errorf("unsupported sense response code %#x", sb[0])
	}
}

// classify maps the CHECK POWER MODE COUNT value to a State (ACS-3 7.3).
func classify(count byte) State {
	switch count {
	case 0x00, 0x01: // Standby_z, Standby_y
		return StateStandby
	case 0x40: // obsolete: NV cache power mode, spindle spun down
		return StateStandby
	case 0x41, 0x80, 0x81, 0x82, 0x83, 0xff: // NV cache spun up, idle, active
		return StateActive
	default:
		return StateUnknown
	}
}

// decode turns raw sense data into a power state.
func decode(sb []byte) (State, error) {
	status, count, err := parseSense(sb)
	if err != nil {
		return StateUnknown, err
	}
	if status&ataStatusErr != 0 {
		return StateUnknown, fmt.Errorf("ATA status %#x reports an error", status)
	}
	s := classify(count)
	if s == StateUnknown {
		return s, fmt.Errorf("unknown power mode %#x", count)
	}
	return s, nil
}
