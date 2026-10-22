// Parted Magic USB installer (working name pmusb).
//
// Copyright 2026 Patrick J. Verner, Waupaca, WI, USA
//
// This program is free software; you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation; either version 2 of the License, or (at your option)
// any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU General Public License for
// more details. You should have received a copy of the GNU General Public
// License along with this program; if not, see <https://www.gnu.org/licenses/>.

//go:build android || pmusbfs

package main

// Android device layer. An Android app cannot open a disk, but the USB API
// gives it a usbfs file descriptor for a stick the user has allowed. The
// app claims the stick's mass-storage interface (which takes it away from
// the kernel's own driver) and hands this program that descriptor, and the
// ISO's from the system file picker, over a local socket named in
// PMUSB_FD_SOCKET. This file then speaks USB Mass Storage Bulk-Only
// Transport and the SCSI commands a stick needs, through usbfs ioctls.
//
// The same code builds for Linux with -tags pmusbfs, so it can be run
// against a real stick on a PC, with a small sender doing the app's part.
//
// The target is named "usb:" and the ISO "fd:iso".

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// <linux/usbdevice_fs.h> on a 64-bit kernel (the same on arm64 and x86_64).
const (
	usbdevfsControl   = 0xc0185500
	usbdevfsBulk      = 0xc0185502
	usbdevfsClaim     = 0x8004550f
	usbdevfsClearHalt = 0x80045515
)

const (
	usbTarget  = "usb:"
	isoSource  = "fd:iso"
	cbwSig     = 0x43425355 // "USBC"
	cswSig     = 0x53425355 // "USBS"
	bulkChunk  = 64 << 10   // one usbfs transfer; larger ones need a large kernel buffer
	cmdTimeout = 10000      // ms, for the command and status phases
	ioTimeout  = 30000      // ms, per data chunk
)

type Device struct {
	Path, Model, Vendor, Serial string
	Size                        int64
	Removable, ReadOnly, USB    bool
	SectorSize                  int
}

// ---- the handoff from the app ----

type handoff struct {
	usb, iso    int // descriptors, -1 when not handed over
	epIn, epOut uint32
	iface       uint32
	serial      string
	err         error
}

var (
	handOnce sync.Once
	hand     handoff
)

// getHandoff connects once to the app's socket and reads one message: a
// line of key=value words and the descriptors it names, by position.
//
//	usb=0 iso=1 in=0x81 out=0x02 iface=0 serial=4C530001240719113593
func getHandoff() *handoff {
	handOnce.Do(func() {
		hand = handoff{usb: -1, iso: -1}
		name := os.Getenv("PMUSB_FD_SOCKET")
		if name == "" {
			hand.err = errors.New("no USB drive was handed to the installer (PMUSB_FD_SOCKET is not set)")
			return
		}
		c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: "@" + name, Net: "unix"})
		if err != nil {
			hand.err = fmt.Errorf("connect to the app: %w", err)
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		buf := make([]byte, 1024)
		oob := make([]byte, unix.CmsgSpace(4*4))
		n, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
		if err != nil {
			hand.err = fmt.Errorf("read from the app: %w", err)
			return
		}
		var fds []int
		if msgs, perr := unix.ParseSocketControlMessage(oob[:oobn]); perr == nil {
			for i := range msgs {
				if f, ferr := unix.ParseUnixRights(&msgs[i]); ferr == nil {
					fds = append(fds, f...)
				}
			}
		}
		pick := func(v string) int {
			i, err := strconv.Atoi(v)
			if err != nil || i < 0 || i >= len(fds) {
				return -1
			}
			return fds[i]
		}
		num := func(v string) uint32 {
			x, _ := strconv.ParseUint(v, 0, 32)
			return uint32(x)
		}
		for _, w := range strings.Fields(string(buf[:n])) {
			k, v, ok := strings.Cut(w, "=")
			if !ok {
				continue
			}
			switch k {
			case "usb":
				hand.usb = pick(v)
			case "iso":
				hand.iso = pick(v)
			case "in":
				hand.epIn = num(v)
			case "out":
				hand.epOut = num(v)
			case "iface":
				hand.iface = num(v)
			case "serial":
				hand.serial = v
			}
		}
	})
	return &hand
}

func init() {
	sourceFile = func(p string) (*os.File, error) {
		if p != isoSource {
			return nil, nil
		}
		h := getHandoff()
		if h.err != nil {
			return nil, h.err
		}
		if h.iso < 0 {
			return nil, errors.New("no ISO was handed to the installer")
		}
		// A duplicate, so closing the source leaves the handed one open.
		fd, err := unix.Dup(h.iso)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), "iso"), nil
	}
}

// ---- Bulk-Only Transport over usbfs ----

type bulkXfer struct {
	ep, length, timeout, _ uint32
	data                   uintptr
}

type ctrlXfer struct {
	reqType, req         uint8
	value, index, length uint16
	timeout              uint32
	data                 uintptr
}

type senseError struct {
	op             byte
	key, asc, ascq byte
}

func (e *senseError) Error() string {
	return fmt.Sprintf("SCSI command %#02x failed: sense key %#x, ASC %#02x, ASCQ %#02x", e.op, e.key, e.asc, e.ascq)
}

// usbMS is one claimed mass-storage interface. Its buffers live in this
// heap object, so the kernel is never handed a pointer into a stack.
type usbMS struct {
	fd       int
	in, out  uint32
	iface    uint32
	tag      uint32
	cbw      [31]byte
	csw      [13]byte
	small    [256]byte
	blockLen int
	blocks   uint64
}

func (u *usbMS) bulk(ep uint32, b []byte, ms uint32) (int, error) {
	x := bulkXfer{ep: ep, length: uint32(len(b)), timeout: ms}
	if len(b) > 0 {
		x.data = uintptr(unsafe.Pointer(&b[0]))
	}
	r, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(u.fd), usbdevfsBulk, uintptr(unsafe.Pointer(&x)))
	runtime.KeepAlive(b)
	if e != 0 {
		return 0, e
	}
	return int(r), nil
}

func (u *usbMS) clearHalt(ep uint32) {
	e := ep
	unix.Syscall(unix.SYS_IOCTL, uintptr(u.fd), usbdevfsClearHalt, uintptr(unsafe.Pointer(&e)))
}

// resetRecovery is the Bulk-Only Mass Storage Reset and both Clear
// Features the spec asks for after a phase error.
func (u *usbMS) resetRecovery() {
	x := ctrlXfer{reqType: 0x21, req: 0xff, index: uint16(u.iface), timeout: cmdTimeout}
	unix.Syscall(unix.SYS_IOCTL, uintptr(u.fd), usbdevfsControl, uintptr(unsafe.Pointer(&x)))
	u.clearHalt(u.in)
	u.clearHalt(u.out)
}

// transfer moves a data phase in chunks; the stick sees one stream.
func (u *usbMS) transfer(ep uint32, b []byte) (int, error) {
	done := 0
	for done < len(b) {
		want := min(bulkChunk, len(b)-done)
		n, err := u.bulk(ep, b[done:done+want], ioTimeout)
		done += n
		if err != nil {
			return done, err
		}
		if n < want { // a short packet ends the data phase
			return done, nil
		}
	}
	return done, nil
}

func (u *usbMS) readCSW() error {
	n, err := u.bulk(u.in, u.csw[:], cmdTimeout)
	if errors.Is(err, unix.EPIPE) {
		u.clearHalt(u.in)
		n, err = u.bulk(u.in, u.csw[:], cmdTimeout)
	}
	if err != nil {
		return err
	}
	if n != 13 || binary.LittleEndian.Uint32(u.csw[0:]) != cswSig || binary.LittleEndian.Uint32(u.csw[4:]) != u.tag {
		u.resetRecovery()
		return errors.New("the drive sent a bad status block")
	}
	return nil
}

// command runs one SCSI command. data is read when in is true.
func (u *usbMS) command(cdb []byte, data []byte, in bool, sense bool) error {
	u.tag++
	c := u.cbw[:]
	binary.LittleEndian.PutUint32(c[0:], cbwSig)
	binary.LittleEndian.PutUint32(c[4:], u.tag)
	binary.LittleEndian.PutUint32(c[8:], uint32(len(data)))
	c[12] = 0
	if in {
		c[12] = 0x80
	}
	c[13] = 0
	c[14] = byte(len(cdb))
	clear(c[15:])
	copy(c[15:], cdb)
	if n, err := u.bulk(u.out, c, cmdTimeout); err != nil || n != len(c) {
		if errors.Is(err, unix.ENODEV) {
			return errors.New("the drive was disconnected")
		}
		u.resetRecovery()
		return fmt.Errorf("send command %#02x: %v", cdb[0], err)
	}
	if len(data) > 0 {
		ep := u.out
		if in {
			ep = u.in
		}
		if _, err := u.transfer(ep, data); err != nil {
			if errors.Is(err, unix.ENODEV) {
				return errors.New("the drive was disconnected")
			}
			if !errors.Is(err, unix.EPIPE) {
				u.resetRecovery()
				return fmt.Errorf("command %#02x data: %v", cdb[0], err)
			}
			u.clearHalt(ep)
		}
	}
	if err := u.readCSW(); err != nil {
		if errors.Is(err, unix.ENODEV) {
			return errors.New("the drive was disconnected")
		}
		return fmt.Errorf("command %#02x status: %v", cdb[0], err)
	}
	residue := binary.LittleEndian.Uint32(u.csw[8:])
	switch u.csw[12] {
	case 0:
		if residue != 0 && !in {
			return fmt.Errorf("command %#02x: the drive took %d bytes less than sent", cdb[0], residue)
		}
		if residue != 0 && in && len(data) > 0 && cdb[0] != 0x12 && cdb[0] != 0x1a && cdb[0] != 0x03 {
			return fmt.Errorf("command %#02x: the drive sent %d bytes less than asked", cdb[0], residue)
		}
		return nil
	case 1:
		if !sense {
			return &senseError{op: cdb[0]}
		}
		return u.requestSense(cdb[0])
	default:
		u.resetRecovery()
		return fmt.Errorf("command %#02x: phase error", cdb[0])
	}
}

func (u *usbMS) requestSense(op byte) error {
	b := u.small[:18]
	clear(b)
	if err := u.command([]byte{0x03, 0, 0, 0, 18, 0}, b, true, false); err != nil {
		return &senseError{op: op}
	}
	return &senseError{op: op, key: b[2] & 0x0f, asc: b[12], ascq: b[13]}
}

func (u *usbMS) testUnitReady() error {
	var err error
	for i := 0; i < 20; i++ {
		if err = u.command(make([]byte, 6), nil, false, true); err == nil {
			return nil
		}
		var se *senseError
		if !errors.As(err, &se) {
			return err
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("the drive is not ready: %v", err)
}

func trimSCSI(b []byte) string {
	return strings.TrimSpace(strings.Trim(string(b), "\x00"))
}

func (u *usbMS) inquiry() (vendor, product string, removable bool, err error) {
	b := u.small[:36]
	clear(b)
	if err = u.command([]byte{0x12, 0, 0, 0, 36, 0}, b, true, true); err != nil {
		return
	}
	return trimSCSI(b[8:16]), trimSCSI(b[16:32]), b[1]&0x80 != 0, nil
}

func (u *usbMS) readCapacity() error {
	b := u.small[:8]
	clear(b)
	if err := u.command([]byte{0x25, 0, 0, 0, 0, 0, 0, 0, 0, 0}, b, true, true); err != nil {
		return err
	}
	last := uint64(binary.BigEndian.Uint32(b[0:]))
	u.blockLen = int(binary.BigEndian.Uint32(b[4:]))
	if last == 0xffffffff {
		b = u.small[:32]
		clear(b)
		cdb := []byte{0x9e, 0x10, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 32, 0, 0}
		if err := u.command(cdb, b, true, true); err != nil {
			return err
		}
		last = binary.BigEndian.Uint64(b[0:])
		u.blockLen = int(binary.BigEndian.Uint32(b[8:]))
	}
	u.blocks = last + 1
	if u.blockLen <= 0 {
		return errors.New("the drive reported no block size")
	}
	return nil
}

// writeProtected reads the WP bit from MODE SENSE(6). A drive that refuses
// the command is taken as writable; the first write would say otherwise.
func (u *usbMS) writeProtected() bool {
	b := u.small[:4]
	clear(b)
	if err := u.command([]byte{0x1a, 0, 0x3f, 0, 4, 0}, b, true, true); err != nil {
		return false
	}
	return b[2]&0x80 != 0
}

func (u *usbMS) rw(write bool, lba uint64, b []byte) error {
	count := uint64(len(b) / u.blockLen)
	var cdb []byte
	if lba+count <= 0xffffffff && count <= 0xffff {
		op := byte(0x28)
		if write {
			op = 0x2a
		}
		cdb = []byte{op, 0, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(cdb[2:], uint32(lba))
		binary.BigEndian.PutUint16(cdb[7:], uint16(count))
	} else {
		op := byte(0x88)
		if write {
			op = 0x8a
		}
		cdb = make([]byte, 16)
		cdb[0] = op
		binary.BigEndian.PutUint64(cdb[2:], lba)
		binary.BigEndian.PutUint32(cdb[10:], uint32(count))
	}
	return u.command(cdb, b, !write, true)
}

// syncCache asks the drive to empty its own cache. Many sticks refuse the
// command (ILLEGAL REQUEST) because they have none; that is not an error.
func (u *usbMS) syncCache() error {
	err := u.command([]byte{0x35, 0, 0, 0, 0, 0, 0, 0, 0, 0}, nil, false, true)
	var se *senseError
	if errors.As(err, &se) && se.key == 0x05 {
		return nil
	}
	return err
}

// eject sends START STOP UNIT with LoEj, so the phone does not mount the
// finished stick and write to it.
func (u *usbMS) eject() error {
	return u.command([]byte{0x1b, 0, 0, 0, 0x02, 0}, nil, false, true)
}

func openUSB() (*usbMS, *Device, error) {
	h := getHandoff()
	if h.err != nil {
		return nil, nil, h.err
	}
	if h.usb < 0 || h.epIn == 0 || h.epOut == 0 {
		return nil, nil, errors.New("no USB drive was handed to the installer")
	}
	u := &usbMS{fd: h.usb, in: h.epIn, out: h.epOut, iface: h.iface}
	// The app has claimed the interface; claiming it again through the same
	// open file is harmless and makes sure.
	ifc := h.iface
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(u.fd), usbdevfsClaim, uintptr(unsafe.Pointer(&ifc))); e != 0 && e != unix.EBUSY {
		return nil, nil, fmt.Errorf("claim the drive: %v", e)
	}
	if err := u.testUnitReady(); err != nil {
		return nil, nil, err
	}
	vendor, product, removable, err := u.inquiry()
	if err != nil {
		return nil, nil, err
	}
	if err := u.readCapacity(); err != nil {
		return nil, nil, err
	}
	d := &Device{
		Path:       usbTarget,
		Vendor:     vendor,
		Model:      strings.TrimSpace(vendor + " " + product),
		Serial:     h.serial,
		Size:       int64(u.blocks) * int64(u.blockLen),
		Removable:  removable,
		ReadOnly:   u.writeProtected(),
		USB:        true,
		SectorSize: u.blockLen,
	}
	return u, d, nil
}

func listDevices(all, fixed bool, sourcePath string) ([]Device, error) {
	_, d, err := openUSB()
	if err != nil {
		return nil, err
	}
	if !fixed && !d.Removable {
		return nil, nil
	}
	return []Device{*d}, nil
}

// ---- the target ----

type usbStick struct {
	u    *usbMS
	size int64
	c    *blockCache
	ro   bool
	mbr  bool
	done bool
}

func (t *usbStick) rawRead(b []byte, off int64) error {
	bl := int64(t.u.blockLen)
	if off%bl != 0 || int64(len(b))%bl != 0 {
		return fmt.Errorf("read at %d: not on a block boundary", off)
	}
	if err := t.u.rw(false, uint64(off/bl), b); err != nil {
		return fmt.Errorf("read at %d: %w", off, err)
	}
	return nil
}

func (t *usbStick) rawWrite(b []byte, off int64) error {
	if t.ro {
		return errors.New("read-only target")
	}
	bl := int64(t.u.blockLen)
	if off%bl != 0 || int64(len(b))%bl != 0 {
		return fmt.Errorf("write at %d: not on a block boundary", off)
	}
	if err := t.u.rw(true, uint64(off/bl), b); err != nil {
		return fmt.Errorf("write at %d: %w", off, err)
	}
	return nil
}

func (t *usbStick) ReadAt(p []byte, off int64) (int, error) { return t.c.ReadAt(p, off) }
func (t *usbStick) WriteAt(p []byte, off int64) (int, error) {
	if t.ro {
		return 0, errors.New("read-only target")
	}
	// The partition table is written last, after verify (install.go).
	if off == 0 && len(p) >= 512 && p[510] == 0x55 && p[511] == 0xaa {
		t.mbr = true
	}
	return t.c.WriteAt(p, off)
}
func (t *usbStick) Size() int64          { return t.size }
func (t *usbStick) Checkpoint() error    { return nil }
func (t *usbStick) SetCacheBlocks(n int) { t.c.SetCacheBlocks(n) }
func (t *usbStick) File() *os.File       { return nil }
func (t *usbStick) IsDevice() bool       { return true }
func (t *usbStick) Sync() error {
	if t.ro {
		return nil
	}
	if err := t.c.flushAll(); err != nil {
		return err
	}
	return t.u.syncCache()
}

var lastEjected bool

func (t *usbStick) Close() error {
	if t.done {
		return nil
	}
	t.done = true
	if t.ro {
		return nil
	}
	err := t.c.flushAll()
	if t.mbr && err == nil {
		if err = t.u.syncCache(); err == nil && t.u.eject() == nil {
			lastEjected = true
		}
	}
	return err
}

func openDevice(path, expect string, allowNonUSB, allowFixed bool, sourcePath string) (Target, *Device, error) {
	if path != usbTarget {
		return nil, nil, errors.New("refused: " + path + " is not the USB drive handed to the installer (use usb:)")
	}
	u, d, err := openUSB()
	if err != nil {
		return nil, nil, err
	}
	if !allowFixed && !d.Removable {
		return nil, nil, errors.New("refused: this drive reports fixed media (use --allow-fixed if you mean it)")
	}
	if d.ReadOnly {
		return nil, nil, errors.New("refused: the drive is write-protected")
	}
	if d.SectorSize != 512 {
		return nil, nil, fmt.Errorf("refused: this drive has %d-byte sectors, which the BIOS boot loader does not support", d.SectorSize)
	}
	if expect != "" {
		got := fmt.Sprintf("%s|%s|%d", d.Model, d.Serial, d.Size)
		if got != expect {
			return nil, nil, fmt.Errorf("refused: device changed since it was chosen (expected %q, found %q)", expect, got)
		}
	}
	t := &usbStick{u: u, size: d.Size}
	t.c = &blockCache{dev: t, size: d.Size}
	return t, d, nil
}

func rereadPartitions(path string) error {
	if lastEjected {
		emit("log", map[string]any{"phase": "finish", "message": "ejected the drive; it can be unplugged"})
	}
	return nil
}

// isDevicePath: the handed-over USB drive.
func isDevicePath(p string) bool { return strings.HasPrefix(p, usbTarget) }

// openReadTarget opens the handed-over drive read-only (verify), or an image.
func openReadTarget(path string) (Target, error) {
	if !isDevicePath(path) {
		return openFileTargetRO(path)
	}
	if path != usbTarget {
		return nil, errors.New(path + " is not the USB drive handed to the installer (use usb:)")
	}
	u, d, err := openUSB()
	if err != nil {
		return nil, err
	}
	t := &usbStick{u: u, size: d.Size, ro: true}
	t.c = &blockCache{dev: t, size: d.Size}
	return t, nil
}
