//go:build linux

package power

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	sgIO          = 0x2285
	sgDxferNone   = -1
	sgTimeoutMsec = 5000
)

// sgIOHdr mirrors struct sg_io_hdr from <scsi/sg.h>. Go's field alignment
// matches the C layout on the 64-bit platforms this tool supports.
type sgIOHdr struct {
	interfaceID    int32
	dxferDirection int32
	cmdLen         uint8
	mxSbLen        uint8
	iovecCount     uint16
	dxferLen       uint32
	dxferp         uintptr
	cmdp           uintptr
	sbp            uintptr
	timeout        uint32
	flags          uint32
	packID         int32
	usrPtr         uintptr
	status         uint8
	maskedStatus   uint8
	msgStatus      uint8
	sbLenWr        uint8
	hostStatus     uint16
	driverStatus   uint16
	resid          int32
	duration       uint32
	info           uint32
}

// Check sends CHECK POWER MODE to the block device at devPath (e.g.
// "/dev/sda") and returns its power state. It does not wake the drive.
func Check(devPath string) (State, error) {
	f, err := os.OpenFile(devPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return StateUnknown, err
	}
	defer func() { _ = f.Close() }()

	// The kernel follows the CDB and sense pointers inside hdr, so pin them:
	// uintptr fields are invisible to the garbage collector.
	cdb := new([16]byte)
	*cdb = checkPowerModeCDB()
	sense := new([32]byte)
	var pin runtime.Pinner
	pin.Pin(cdb)
	pin.Pin(sense)
	defer pin.Unpin()
	hdr := sgIOHdr{
		interfaceID:    'S',
		dxferDirection: sgDxferNone,
		cmdLen:         uint8(len(cdb)),
		mxSbLen:        uint8(len(sense)),
		cmdp:           uintptr(unsafe.Pointer(&cdb[0])),
		sbp:            uintptr(unsafe.Pointer(&sense[0])),
		timeout:        sgTimeoutMsec,
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), sgIO, uintptr(unsafe.Pointer(&hdr)))
	if errno != 0 {
		return StateUnknown, fmt.Errorf("SG_IO on %s: %w", devPath, errno)
	}
	if hdr.hostStatus != 0 {
		return StateUnknown, fmt.Errorf("SG_IO on %s: host status %#x", devPath, hdr.hostStatus)
	}
	if hdr.sbLenWr == 0 {
		return StateUnknown, fmt.Errorf("SG_IO on %s: no sense data returned", devPath)
	}
	return decode(sense[:hdr.sbLenWr])
}
