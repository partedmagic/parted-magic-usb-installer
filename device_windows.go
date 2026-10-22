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

//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// Windows device layer, per design map section 4 step 2 and section 7:
// lock and dismount every volume of the target, open \\.\PhysicalDriveN with
// FILE_FLAG_NO_BUFFERING|FILE_FLAG_WRITE_THROUGH, do all I/O sector-aligned
// through bounce buffers, hold the locks until the end. Written from the
// documented API and USBImager's sequence; needs a Windows test cycle.

const (
	fsctlLockVolume                 = 0x90018
	fsctlDismountVolume             = 0x90020
	fsctlUnlockVolume               = 0x9001C
	ioctlStorageQueryProperty       = 0x2D1400
	ioctlDiskGetDriveGeometryEx     = 0x700A0
	ioctlDiskGetLengthInfo          = 0x7405C
	ioctlDiskUpdateProperties       = 0x70140
	ioctlVolumeGetVolumeDiskExtents = 0x560000
	busTypeUsb                      = 7
)

type Device struct {
	Path, Model, Vendor, Serial string
	Size                        int64
	Removable, ReadOnly, USB    bool
	SectorSize                  int
	Number                      int
	Letters                     string
}

func ioctl(h windows.Handle, code uint32, in []byte, out []byte) (uint32, error) {
	var ret uint32
	var inp, outp *byte
	if len(in) > 0 {
		inp = &in[0]
	}
	if len(out) > 0 {
		outp = &out[0]
	}
	err := windows.DeviceIoControl(h, code, inp, uint32(len(in)), outp, uint32(len(out)), &ret, nil)
	return ret, err
}

func openRaw(path string, access uint32, flags uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, flags, 0)
}

func cstr(b []byte, off uint32) string {
	if off == 0 || int(off) >= len(b) {
		return ""
	}
	end := int(off)
	for end < len(b) && b[end] != 0 {
		end++
	}
	return strings.TrimSpace(string(b[off:end]))
}

func queryDisk(h windows.Handle, n int) (Device, error) {
	d := Device{Path: fmt.Sprintf(`\\.\PhysicalDrive%d`, n), Number: n}
	q := make([]byte, 12) // STORAGE_PROPERTY_QUERY: StorageDeviceProperty, PropertyStandardQuery
	desc := make([]byte, 4096)
	if _, err := ioctl(h, ioctlStorageQueryProperty, q, desc); err != nil {
		return d, err
	}
	le := func(o int) uint32 {
		return uint32(desc[o]) | uint32(desc[o+1])<<8 | uint32(desc[o+2])<<16 | uint32(desc[o+3])<<24
	}
	d.Removable = desc[10] != 0
	d.Vendor = cstr(desc, le(12))
	d.Model = strings.TrimSpace(d.Vendor + " " + cstr(desc, le(16)))
	d.Serial = cstr(desc, le(24))
	d.USB = le(28) == busTypeUsb
	li := make([]byte, 8)
	if _, err := ioctl(h, ioctlDiskGetLengthInfo, nil, li); err != nil {
		return d, err
	}
	d.Size = int64(uint64(li[0]) | uint64(li[1])<<8 | uint64(li[2])<<16 | uint64(li[3])<<24 | uint64(li[4])<<32 | uint64(li[5])<<40 | uint64(li[6])<<48 | uint64(li[7])<<56)
	geo := make([]byte, 64)
	if _, err := ioctl(h, ioctlDiskGetDriveGeometryEx, nil, geo); err == nil {
		d.SectorSize = int(uint32(geo[20]) | uint32(geo[21])<<8 | uint32(geo[22])<<16 | uint32(geo[23])<<24)
	}
	return d, nil
}

// diskNumbersOfVolume returns the physical disks a volume GUID path spans.
func diskNumbersOfVolume(volGUID string) []int {
	h, err := openRaw(strings.TrimSuffix(volGUID, `\`), windows.GENERIC_READ, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(h)
	buf := make([]byte, 8+24*64)
	if _, err := ioctl(h, ioctlVolumeGetVolumeDiskExtents, nil, buf); err != nil && err != windows.ERROR_MORE_DATA {
		return nil
	}
	n := int(uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16 | uint32(buf[3])<<24)
	if n > 64 {
		n = 64
	}
	var out []int
	for i := 0; i < n; i++ {
		o := 8 + 24*i
		out = append(out, int(uint32(buf[o])|uint32(buf[o+1])<<8|uint32(buf[o+2])<<16|uint32(buf[o+3])<<24))
	}
	return out
}

func volumeGUIDForPath(p string) string {
	var mp [windows.MAX_PATH + 1]uint16
	pp, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return ""
	}
	if err := windows.GetVolumePathName(pp, &mp[0], uint32(len(mp))); err != nil {
		return ""
	}
	var vol [64]uint16
	if err := windows.GetVolumeNameForVolumeMountPoint(&mp[0], &vol[0], uint32(len(vol))); err != nil {
		return ""
	}
	return windows.UTF16ToString(vol[:])
}

// allVolumes lists volume GUID paths with their drive letters.
func allVolumes() map[string]string {
	out := map[string]string{}
	var name [64]uint16
	h, err := windows.FindFirstVolume(&name[0], uint32(len(name)))
	if err != nil {
		return out
	}
	defer windows.FindVolumeClose(h)
	for {
		vol := windows.UTF16ToString(name[:])
		var letters string
		var buf [512]uint16
		var ret uint32
		vp, _ := windows.UTF16PtrFromString(vol)
		if err := windows.GetVolumePathNamesForVolumeName(vp, &buf[0], uint32(len(buf)), &ret); err == nil {
			var parts []string
			for _, s := range strings.Split(windows.UTF16ToString(buf[:]), "\x00") {
				if s != "" {
					parts = append(parts, strings.TrimSuffix(s, `\`))
				}
			}
			letters = strings.Join(parts, " ")
		}
		out[vol] = letters
		if err := windows.FindNextVolume(h, &name[0], uint32(len(name))); err != nil {
			break
		}
	}
	return out
}

func protectedDiskNumbers(sourcePath string) map[int]bool {
	prot := map[int]bool{}
	if wd, err := windows.GetWindowsDirectory(); err == nil && wd != "" {
		for _, d := range diskNumbersOfVolume(volumeGUIDForPath(wd)) {
			prot[d] = true
		}
	}
	if sourcePath != "" {
		for _, d := range diskNumbersOfVolume(volumeGUIDForPath(sourcePath)) {
			prot[d] = true
		}
	}
	return prot
}

func listDevices(all, fixed bool, sourcePath string) ([]Device, error) {
	prot := protectedDiskNumbers(sourcePath)
	vols := allVolumes()
	var out []Device
	for n := 0; n < 64; n++ {
		h, err := openRaw(fmt.Sprintf(`\\.\PhysicalDrive%d`, n), windows.GENERIC_READ, 0)
		if err != nil {
			continue
		}
		d, err := queryDisk(h, n)
		windows.CloseHandle(h)
		if err != nil || d.Size == 0 || prot[n] {
			continue
		}
		if !all && !d.USB {
			continue
		}
		if !fixed && d.USB && !d.Removable {
			continue
		}
		var letters []string
		for vol, l := range vols {
			for _, dn := range diskNumbersOfVolume(vol) {
				if dn == n && l != "" {
					letters = append(letters, l)
				}
			}
		}
		d.Letters = strings.Join(letters, " ")
		out = append(out, d)
	}
	return out, nil
}

// winTarget: a raw physical-drive handle behind the shared write-back cache.
type winTarget struct {
	h    windows.Handle
	f    *os.File
	size int64
	vols []windows.Handle
	c    *blockCache
	ro   bool
}

func (t *winTarget) rawRead(b []byte, off int64) error {
	var done uint32
	ov := windows.Overlapped{Offset: uint32(off), OffsetHigh: uint32(off >> 32)}
	if err := windows.ReadFile(t.h, b, &done, &ov); err != nil {
		return fmt.Errorf("read at %d: %w", off, err)
	}
	if int(done) != len(b) {
		return fmt.Errorf("short read at %d: %d of %d", off, done, len(b))
	}
	return nil
}

func (t *winTarget) rawWrite(b []byte, off int64) error {
	var done uint32
	ov := windows.Overlapped{Offset: uint32(off), OffsetHigh: uint32(off >> 32)}
	if err := windows.WriteFile(t.h, b, &done, &ov); err != nil {
		return fmt.Errorf("write at %d: %w", off, err)
	}
	if int(done) != len(b) {
		return fmt.Errorf("short write at %d: %d of %d", off, done, len(b))
	}
	return nil
}

func (t *winTarget) ReadAt(p []byte, off int64) (int, error)  { return t.c.ReadAt(p, off) }
func (t *winTarget) WriteAt(p []byte, off int64) (int, error) { return t.c.WriteAt(p, off) }

func (t *winTarget) Size() int64          { return t.size }
func (t *winTarget) Checkpoint() error    { return nil }
func (t *winTarget) SetCacheBlocks(n int) { t.c.SetCacheBlocks(n) }
func (t *winTarget) Sync() error {
	if err := t.c.flushAll(); err != nil {
		return err
	}
	return windows.FlushFileBuffers(t.h)
}
func (t *winTarget) File() *os.File { return t.f }
func (t *winTarget) IsDevice() bool { return true }
func (t *winTarget) Close() error {
	if t.ro {
		return t.f.Close()
	}
	t.c.flushAll()
	ioctl(t.h, ioctlDiskUpdateProperties, nil, nil)
	for _, v := range t.vols {
		ioctl(v, fsctlUnlockVolume, nil, nil)
		windows.CloseHandle(v)
	}
	t.vols = nil
	return t.f.Close()
}

func openDevice(path, expect string, allowNonUSB, allowFixed bool, sourcePath string) (Target, *Device, error) {
	devs, err := listDevices(allowNonUSB, allowFixed, sourcePath)
	if err != nil {
		return nil, nil, err
	}
	var d *Device
	for i := range devs {
		if strings.EqualFold(devs[i].Path, path) {
			d = &devs[i]
		}
	}
	if d == nil {
		return nil, nil, errors.New("refused: " + path + " is not an eligible USB drive (or it holds Windows or the source ISO)")
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
	t := &winTarget{size: d.Size}
	t.c = &blockCache{dev: t, size: d.Size}
	for vol := range allVolumes() {
		onTarget := false
		for _, dn := range diskNumbersOfVolume(vol) {
			if dn == d.Number {
				onTarget = true
			}
		}
		if !onTarget {
			continue
		}
		vh, err := openRaw(strings.TrimSuffix(vol, `\`), windows.GENERIC_READ|windows.GENERIC_WRITE, 0)
		if err != nil {
			t.Close()
			return nil, nil, fmt.Errorf("open volume %s: %w", vol, err)
		}
		if _, err := ioctl(vh, fsctlLockVolume, nil, nil); err != nil {
			windows.CloseHandle(vh)
			for _, v := range t.vols {
				ioctl(v, fsctlUnlockVolume, nil, nil)
				windows.CloseHandle(v)
			}
			return nil, nil, errors.New("another program is using the drive (volume lock failed); close Explorer windows on it and try again")
		}
		ioctl(vh, fsctlDismountVolume, nil, nil)
		t.vols = append(t.vols, vh)
	}
	h, err := openRaw(d.Path, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_NO_BUFFERING)
	if err != nil {
		emit("log", map[string]any{"phase": "prepare", "message": "unbuffered open failed (" + err.Error() + "); retrying with a plain handle"})
		h, err = openRaw(d.Path, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_ATTRIBUTE_NORMAL)
	}
	if err != nil {
		for _, v := range t.vols {
			ioctl(v, fsctlUnlockVolume, nil, nil)
			windows.CloseHandle(v)
		}
		return nil, nil, fmt.Errorf("open %s: %w (run as administrator)", d.Path, err)
	}
	t.h = h
	t.f = os.NewFile(uintptr(h), d.Path)
	return t, d, nil
}

func rereadPartitions(path string) error { return nil }

// isDevicePath: a physical drive path; os.Stat cannot see those.
func isDevicePath(p string) bool {
	l := strings.ToLower(p)
	return strings.HasPrefix(l, `\\.\physicaldrive`) || strings.HasPrefix(l, `\\?\physicaldrive`)
}

// openReadTarget opens a physical drive read-only through the cache (verify).
func openReadTarget(path string) (Target, error) {
	if !isDevicePath(path) {
		return openFileTargetRO(path)
	}
	h, err := openRaw(path, windows.GENERIC_READ, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_NO_BUFFERING)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w (run as administrator)", path, err)
	}
	li := make([]byte, 8)
	if _, err := ioctl(h, ioctlDiskGetLengthInfo, nil, li); err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	size := int64(uint64(li[0]) | uint64(li[1])<<8 | uint64(li[2])<<16 | uint64(li[3])<<24 | uint64(li[4])<<32 | uint64(li[5])<<40 | uint64(li[6])<<48 | uint64(li[7])<<56)
	t := &winTarget{h: h, f: os.NewFile(uintptr(h), path), size: size, ro: true}
	t.c = &blockCache{dev: t, size: size}
	return t, nil
}
