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

//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// macOS device layer. Drives are found by walking /dev/diskN and asking
// `diskutil info -plist` about each one: `diskutil list` hangs over SSH on
// macOS 26.5 while `info` answers at once (the Air, 2026-10-21). The serial
// comes from the USB node above the disk in the I/O Registry (`ioreg -a`).
// Writes go through /dev/rdiskN, the raw node, which only takes
// sector-aligned transfers, so all I/O passes through the shared block cache
// as on Windows.

// <sys/disk.h>, read from the macOS 26.5 SDK on the Air.
const (
	dkiocGetBlockSize     = 0x40046418
	dkiocGetBlockCount    = 0x40086419
	dkiocSynchronizeCache = 0x20006416
	dkiocEject            = 0x20006415
	diskutilTimeout       = 15 * time.Second
	ioregTimeout          = 15 * time.Second
	unmountTimeout        = 20 * time.Second
)

type Device struct {
	Path, Model, Vendor, Serial string
	Size                        int64
	Removable, ReadOnly, USB    bool
	SectorSize                  int
}

var wholeDiskRe = regexp.MustCompile(`^(?:/dev/)?r?(disk[0-9]+)$`)
var anyDiskRe = regexp.MustCompile(`^(?:/dev/)?r?(disk[0-9]+)(?:s[0-9]+)*$`)

// runTimed runs a command with a deadline; diskutil and ioreg can hang.
func runTimed(d time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("%s %s did not answer within %s", name, strings.Join(args, " "), d)
	}
	return out, err
}

// ---- property lists ----

// parsePlist decodes an XML property list into map[string]any, []any,
// string, int64, float64, bool and []byte.
func parsePlist(b []byte) (any, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("plist: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local != "plist" {
			return plistValue(dec, se)
		}
	}
}

func plistValue(dec *xml.Decoder, se xml.StartElement) (any, error) {
	switch se.Name.Local {
	case "dict":
		m := map[string]any{}
		key := ""
		for {
			tok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				if t.Name.Local == "key" {
					var k string
					if err := dec.DecodeElement(&k, &t); err != nil {
						return nil, err
					}
					key = k
					continue
				}
				v, err := plistValue(dec, t)
				if err != nil {
					return nil, err
				}
				m[key] = v
			case xml.EndElement:
				return m, nil
			}
		}
	case "array":
		var a []any
		for {
			tok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				v, err := plistValue(dec, t)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			case xml.EndElement:
				return a, nil
			}
		}
	case "true", "false":
		if err := dec.Skip(); err != nil {
			return nil, err
		}
		return se.Name.Local == "true", nil
	}
	var s string
	if err := dec.DecodeElement(&s, &se); err != nil {
		return nil, err
	}
	switch se.Name.Local {
	case "integer":
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			u, uerr := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
			if uerr != nil {
				return nil, err
			}
			return int64(u), nil
		}
		return n, nil
	case "real":
		return strconv.ParseFloat(strings.TrimSpace(s), 64)
	}
	return s, nil
}

func pStr(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return strings.TrimSpace(s)
}

func pInt(m map[string]any, k string) int64 {
	n, _ := m[k].(int64)
	return n
}

func pBool(m map[string]any, k string) bool {
	b, _ := m[k].(bool)
	return b
}

// diskInfo returns `diskutil info -plist` for a disk or slice name.
func diskInfo(name string) (map[string]any, error) {
	out, err := runTimed(diskutilTimeout, "diskutil", "info", "-plist", name)
	if err != nil {
		return nil, fmt.Errorf("diskutil info %s: %w", name, err)
	}
	v, err := parsePlist(out)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("diskutil info %s: unexpected output", name)
	}
	return m, nil
}

// ---- protection ----

// physicalDisksOf maps a device node (a slice, an APFS volume, a whole disk)
// to the physical whole disks behind it, through APFS physical stores.
func physicalDisksOf(node string, out map[string]bool, depth int) {
	if depth > 4 {
		return
	}
	m := anyDiskRe.FindStringSubmatch(node)
	if m == nil {
		return
	}
	info, err := diskInfo(strings.TrimPrefix(node, "/dev/"))
	if err != nil {
		// Unknown: protect the whole disk the name points at.
		out[m[1]] = true
		return
	}
	if stores, ok := info["APFSPhysicalStores"].([]any); ok && len(stores) > 0 {
		for _, s := range stores {
			if sm, ok := s.(map[string]any); ok {
				if id := pStr(sm, "APFSPhysicalStore"); id != "" {
					physicalDisksOf(id, out, depth+1)
				}
			}
		}
		return
	}
	if id := pStr(info, "APFSPhysicalStore"); id != "" && id != pStr(info, "DeviceIdentifier") {
		physicalDisksOf(id, out, depth+1)
		return
	}
	if w := pStr(info, "ParentWholeDisk"); w != "" {
		out[w] = true
		return
	}
	out[m[1]] = true
}

func mountFromOf(path string) string {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return ""
	}
	return unix.ByteSliceToString(st.Mntfromname[:])
}

// protectedDisks: the disks behind the running macOS and the disk holding
// the source.
func protectedDisks(sourcePath string) map[string]bool {
	out := map[string]bool{}
	for _, p := range []string{"/", "/System/Volumes/Data"} {
		if n := mountFromOf(p); n != "" {
			physicalDisksOf(n, out, 0)
		}
	}
	if sourcePath != "" {
		if n := mountFromOf(sourcePath); n != "" {
			physicalDisksOf(n, out, 0)
		}
	}
	return out
}

// mountsOf lists the mount points of a whole disk's slices.
func mountsOf(name string) []string {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil || n == 0 {
		return nil
	}
	buf := make([]unix.Statfs_t, n)
	if n, err = unix.Getfsstat(buf, unix.MNT_NOWAIT); err != nil {
		return nil
	}
	var out []string
	for _, st := range buf[:n] {
		from := unix.ByteSliceToString(st.Mntfromname[:])
		if from == "/dev/"+name || strings.HasPrefix(from, "/dev/"+name+"s") {
			out = append(out, unix.ByteSliceToString(st.Mntonname[:]))
		}
	}
	return out
}

// ---- USB serials ----

// usbSerials maps whole-disk names to the USB serial and vendor of the
// device they sit under in the I/O Registry.
// It also returns the USB drives that have a block storage driver but no
// disk under it: plugged in with no medium. A stick ejected through
// DKIOCEJECT stays in that state, and so did one on the Air after a replug
// (2026-10-21) until it was unplugged and plugged in again.
func usbSerials() (map[string][2]string, []string, error) {
	out := map[string][2]string{}
	var noMedium []string
	b, err := runTimed(ioregTimeout, "ioreg", "-a", "-r", "-c", "IOUSBHostDevice", "-l")
	if err != nil {
		return out, nil, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return out, nil, nil
	}
	v, err := parsePlist(b)
	if err != nil {
		return out, nil, err
	}
	roots, _ := v.([]any)
	for _, r := range roots {
		dev, ok := r.(map[string]any)
		if !ok {
			continue
		}
		serial := pStr(dev, "USB Serial Number")
		if serial == "" {
			serial = pStr(dev, "kUSBSerialNumberString")
		}
		vendor := pStr(dev, "USB Vendor Name")
		if vendor == "" {
			vendor = pStr(dev, "kUSBVendorString")
		}
		hasDriver, hasDisk := false, false
		var walk func(n map[string]any, depth int)
		walk = func(n map[string]any, depth int) {
			if depth > 32 {
				return
			}
			if pStr(n, "IOObjectClass") == "IOBlockStorageDriver" {
				hasDriver = true
			}
			if bsd := pStr(n, "BSD Name"); wholeDiskRe.MatchString(bsd) {
				hasDisk = true
				if _, seen := out[bsd]; !seen {
					out[bsd] = [2]string{serial, vendor}
				}
			}
			kids, _ := n["IORegistryEntryChildren"].([]any)
			for _, k := range kids {
				if km, ok := k.(map[string]any); ok {
					walk(km, depth+1)
				}
			}
		}
		walk(dev, 0)
		if hasDriver && !hasDisk {
			name := strings.TrimSpace(vendor + " " + pStr(dev, "USB Product Name"))
			if name == "" {
				name = "A USB drive"
			}
			if serial != "" {
				name += " (serial " + serial + ")"
			}
			noMedium = append(noMedium, name)
		}
	}
	return out, noMedium, nil
}

// quietListing keeps the no-medium notes out of an install's log, where
// openDevice lists the drives again.
var quietListing bool

// ---- listing ----

func wholeDisks() []string {
	ents, err := os.ReadDir("/dev")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if m := wholeDiskRe.FindStringSubmatch(e.Name()); m != nil && !strings.HasPrefix(e.Name(), "r") {
			out = append(out, m[1])
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := strconv.Atoi(strings.TrimPrefix(out[i], "disk"))
		b, _ := strconv.Atoi(strings.TrimPrefix(out[j], "disk"))
		return a < b
	})
	return out
}

func listDevices(all, fixed bool, sourcePath string) ([]Device, error) {
	prot := protectedDisks(sourcePath)
	serials, noMedium, serr := usbSerials()
	if serr != nil {
		emit("log", map[string]any{"phase": "devices", "message": "USB serial numbers unavailable: " + serr.Error()})
	}
	if !quietListing {
		for _, n := range noMedium {
			emit("nomedium", map[string]any{"phase": "devices", "name": n,
				"message": n + " is plugged in but not ready. Unplug it, wait a few seconds and plug it in again."})
		}
	}
	var out []Device
	for _, n := range wholeDisks() {
		if prot[n] {
			continue
		}
		info, err := diskInfo(n)
		if err != nil {
			emit("log", map[string]any{"phase": "devices", "message": err.Error()})
			continue
		}
		// Disk images, APFS containers and other synthesized disks are not drives.
		if pStr(info, "VirtualOrPhysical") == "Virtual" || !pBool(info, "WholeDisk") {
			continue
		}
		d := Device{
			Path:       "/dev/" + n,
			USB:        pStr(info, "BusProtocol") == "USB",
			Size:       pInt(info, "TotalSize"),
			Removable:  pBool(info, "RemovableMedia") || pBool(info, "Removable"),
			ReadOnly:   !pBool(info, "WritableMedia"),
			SectorSize: int(pInt(info, "DeviceBlockSize")),
		}
		if d.Size == 0 {
			d.Size = pInt(info, "Size")
		}
		if !all && !d.USB {
			continue
		}
		if !fixed && d.USB && !d.Removable {
			continue
		}
		if s, ok := serials[n]; ok {
			d.Serial, d.Vendor = s[0], s[1]
		}
		media := pStr(info, "MediaName")
		if media == "" {
			media = strings.TrimSuffix(pStr(info, "IORegistryEntryName"), " Media")
		}
		d.Model = strings.TrimSpace(d.Vendor + " " + media)
		// A non-USB disk with anything mounted is somebody's data disk, not a target.
		if !d.USB && len(mountsOf(n)) > 0 {
			continue
		}
		if d.Size == 0 {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// ---- the raw target ----

type macTarget struct {
	f    *os.File
	size int64
	c    *blockCache
	ro   bool
	// mbr is set when sector 0 receives a boot signature, which the install
	// writes last, after verify. Close then ejects the media through this
	// handle before closing it: once the handle closes, macOS mounts the
	// new volume at once and writes .fseventsd and .Spotlight-V100 onto it
	// (the Air, 2026-10-21), and diskutil eject may never answer.
	mbr     bool
	ejected bool
}

func (t *macTarget) rawRead(b []byte, off int64) error {
	n, err := t.f.ReadAt(b, off)
	if err != nil && !(err == io.EOF && n == len(b)) {
		return fmt.Errorf("read at %d: %w", off, err)
	}
	if n != len(b) {
		return fmt.Errorf("short read at %d: %d of %d", off, n, len(b))
	}
	return nil
}

func (t *macTarget) rawWrite(b []byte, off int64) error {
	if t.ro {
		return errors.New("read-only target")
	}
	n, err := t.f.WriteAt(b, off)
	if err != nil {
		return fmt.Errorf("write at %d: %w", off, err)
	}
	if n != len(b) {
		return fmt.Errorf("short write at %d: %d of %d", off, n, len(b))
	}
	return nil
}

func (t *macTarget) ReadAt(p []byte, off int64) (int, error) { return t.c.ReadAt(p, off) }
func (t *macTarget) WriteAt(p []byte, off int64) (int, error) {
	if t.ro {
		return 0, errors.New("read-only target")
	}
	if off == 0 && len(p) >= 512 && p[510] == 0x55 && p[511] == 0xAA {
		t.mbr = true
	}
	return t.c.WriteAt(p, off)
}
func (t *macTarget) Size() int64          { return t.size }
func (t *macTarget) Checkpoint() error    { return nil }
func (t *macTarget) SetCacheBlocks(n int) { t.c.SetCacheBlocks(n) }
func (t *macTarget) Sync() error {
	if t.ro {
		return nil
	}
	if err := t.c.flushAll(); err != nil {
		return err
	}
	// The raw node has no page cache; this asks the drive to empty its own.
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, t.f.Fd(), dkiocSynchronizeCache, 0); e != 0 {
		return fmt.Errorf("synchronize cache: %w", e)
	}
	return nil
}
func (t *macTarget) File() *os.File { return t.f }
func (t *macTarget) IsDevice() bool { return true }
func (t *macTarget) Close() error {
	if t.f == nil {
		return nil
	}
	if !t.ro {
		t.c.flushAll()
		if t.mbr {
			unix.Syscall(unix.SYS_IOCTL, t.f.Fd(), dkiocSynchronizeCache, 0)
			if _, _, e := unix.Syscall(unix.SYS_IOCTL, t.f.Fd(), dkiocEject, 0); e == 0 {
				t.ejected = true
				lastEjected = true
			} else {
				emit("log", map[string]any{"phase": "finish", "message": "eject through the open handle failed: " + e.Error()})
			}
		}
	}
	err := t.f.Close()
	t.f = nil
	return err
}

// lastEjected tells rereadPartitions that Close already ejected the drive.
var lastEjected bool

// openRaw opens a raw disk node. As root it opens it directly. Otherwise it
// asks /usr/libexec/authopen, the macOS tool made for this: authopen shows
// the system password dialog, opens the node itself and passes the open
// descriptor back over a socket (-stdoutpipe). A root process started by
// the window through `do shell script ... with administrator privileges`
// was refused the raw node (EPERM, the Air, 2026-10-21) while the same
// command over SSH was not, so the window runs the core as the user and
// the core asks authopen.
func openRaw(node string, flags int) (*os.File, error) {
	if os.Geteuid() == 0 {
		f, err := os.OpenFile(node, flags, 0)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w (still mounted or in use?)", node, err)
		}
		return f, nil
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("authopen socket: %w", err)
	}
	ours := os.NewFile(uintptr(pair[0]), "authopen-parent")
	theirs := os.NewFile(uintptr(pair[1]), "authopen-child")
	defer ours.Close()
	cmd := exec.Command("/usr/libexec/authopen", "-stdoutpipe", "-o", strconv.Itoa(flags), node)
	cmd.Stdout = theirs
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		theirs.Close()
		return nil, fmt.Errorf("authopen: %w", err)
	}
	theirs.Close()
	buf := make([]byte, 64)
	oob := make([]byte, unix.CmsgSpace(4))
	_, oobn, _, _, rerr := unix.Recvmsg(pair[0], buf, oob, 0)
	werr := cmd.Wait()
	if rerr == nil && oobn > 0 {
		if msgs, perr := unix.ParseSocketControlMessage(oob[:oobn]); perr == nil && len(msgs) > 0 {
			if fds, ferr := unix.ParseUnixRights(&msgs[0]); ferr == nil && len(fds) > 0 {
				for _, extra := range fds[1:] {
					unix.Close(extra)
				}
				return os.NewFile(uintptr(fds[0]), node), nil
			}
		}
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" && werr != nil {
		msg = werr.Error()
	}
	if msg == "" {
		msg = "no descriptor came back"
	}
	return nil, fmt.Errorf("could not open %s: the password was not given or was refused (authopen: %s)", node, msg)
}

func rawSize(f *os.File) (int64, int, error) {
	var bs uint32
	var bc uint64
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), dkiocGetBlockSize, uintptr(unsafe.Pointer(&bs))); e != 0 {
		return 0, 0, fmt.Errorf("block size: %w", e)
	}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), dkiocGetBlockCount, uintptr(unsafe.Pointer(&bc))); e != 0 {
		return 0, 0, fmt.Errorf("block count: %w", e)
	}
	return int64(bc) * int64(bs), int(bs), nil
}

// unmountDisk unmounts every volume on the disk. It asks Disk Arbitration
// first (diskutil, what Finder uses, so the desktop forgets the volumes
// cleanly). On the Air (2026-10-21) Disk Arbitration answered nothing:
// diskutil list, unmountDisk and unmountDisk force all hung while Spotlight
// held the stick open, and only a forced unmount(2) freed it, at once. The
// drive is about to be erased, so forcing is safe.
func unmountDisk(name string) error {
	mps := mountsOf(name)
	if len(mps) == 0 {
		return nil
	}
	out, err := runTimed(unmountTimeout, "diskutil", "unmountDisk", "/dev/"+name)
	if err == nil && len(mountsOf(name)) == 0 {
		emit("log", map[string]any{"phase": "prepare", "message": "unmounted " + strings.Join(mps, ", ")})
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" && err != nil {
		msg = err.Error()
	}
	emit("log", map[string]any{"phase": "prepare", "message": "diskutil could not unmount the drive (" + msg + "); forcing it"})
	for _, mp := range mountsOf(name) {
		if uerr := unix.Unmount(mp, unix.MNT_FORCE); uerr != nil {
			return fmt.Errorf("could not unmount %s: %v. Close programs and Finder windows using it and try again", mp, uerr)
		}
	}
	if left := mountsOf(name); len(left) > 0 {
		return fmt.Errorf("could not unmount %s: still mounted. Close programs and Finder windows using it and try again", strings.Join(left, ", "))
	}
	emit("log", map[string]any{"phase": "prepare", "message": "unmounted " + strings.Join(mps, ", ") + " (forced)"})
	return nil
}

func openDevice(path, expect string, allowNonUSB, allowFixed bool, sourcePath string) (Target, *Device, error) {
	m := wholeDiskRe.FindStringSubmatch(path)
	if m == nil {
		return nil, nil, errors.New("refused: " + path + " is not a whole disk (use /dev/diskN)")
	}
	name := m[1]
	if _, err := os.Stat("/dev/" + name); err != nil {
		return nil, nil, errors.New("refused: /dev/" + name + " does not exist")
	}
	quietListing = true
	devs, err := listDevices(allowNonUSB, allowFixed, sourcePath)
	quietListing = false
	if err != nil {
		return nil, nil, err
	}
	var d *Device
	for i := range devs {
		if devs[i].Path == "/dev/"+name {
			d = &devs[i]
		}
	}
	if d == nil {
		if prot := protectedDisks(sourcePath); prot[name] {
			return nil, nil, errors.New("refused: /dev/" + name + " holds this Mac's system or the source ISO")
		}
		return nil, nil, errors.New("refused: /dev/" + name + " is not an eligible USB drive (use --allow-non-usb or --allow-fixed if you mean it)")
	}
	if d.ReadOnly {
		return nil, nil, errors.New("refused: device is read-only (write-protect switch?)")
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
	if err := unmountDisk(name); err != nil {
		return nil, nil, err
	}
	f, err := openRaw("/dev/r"+name, os.O_RDWR)
	if err != nil {
		return nil, nil, err
	}
	size, bs, err := rawSize(f)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if bs != 512 {
		f.Close()
		return nil, nil, fmt.Errorf("refused: this drive has %d-byte sectors, which the BIOS boot loader does not support", bs)
	}
	t := &macTarget{f: f, size: size}
	t.c = &blockCache{dev: t, size: size}
	return t, d, nil
}

// rereadPartitions ejects the finished drive. Mounting it would let macOS
// write .fseventsd and .Spotlight-V100 onto it (seen on the Air,
// 2026-10-21), and the drive is meant for another computer.
func rereadPartitions(path string) error {
	m := wholeDiskRe.FindStringSubmatch(path)
	if m == nil {
		return nil
	}
	if lastEjected {
		emit("log", map[string]any{"phase": "finish", "message": "ejected /dev/" + m[1] + "; it can be unplugged"})
		return nil
	}
	out, err := runTimed(unmountTimeout, "diskutil", "eject", "/dev/"+m[1])
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return errors.New("eject: " + msg + " (eject it in Finder before unplugging it)")
	}
	emit("log", map[string]any{"phase": "finish", "message": "ejected /dev/" + m[1] + "; it can be unplugged"})
	return nil
}

// isDevicePath: anything under /dev. A partition (/dev/disk4s1) or a disk
// that is not there must reach openDevice and be refused, never be taken
// for an image file and opened with O_TRUNC.
func isDevicePath(p string) bool {
	return strings.HasPrefix(filepath.Clean(p), "/dev/")
}

// openReadTarget opens a drive read-only through the raw node (verify).
func openReadTarget(path string) (Target, error) {
	if !isDevicePath(path) {
		return openFileTargetRO(path)
	}
	m := wholeDiskRe.FindStringSubmatch(path)
	if m == nil {
		return nil, errors.New(path + " is not a whole disk (use /dev/diskN)")
	}
	f, err := openRaw("/dev/r"+m[1], os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	size, _, err := rawSize(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	t := &macTarget{f: f, size: size, ro: true}
	t.c = &blockCache{dev: t, size: size}
	return t, nil
}
