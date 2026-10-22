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

//go:build linux && !android && !pmusbfs

package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type Device struct {
	Path, Model, Vendor, Serial string
	Size                        int64
	Removable, ReadOnly, USB    bool
	SectorSize                  int
}

func sysRead(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// diskNamesForDevno maps a st_dev to the whole-disk names behind it, through
// partitions and device-mapper slaves.
func diskNamesForDevno(dev uint64, out map[string]bool) {
	p, err := filepath.EvalSymlinks(fmt.Sprintf("/sys/dev/block/%d:%d", unix.Major(dev), unix.Minor(dev)))
	if err != nil {
		return
	}
	diskNamesForSysPath(p, out, 0)
}

func diskNamesForSysPath(p string, out map[string]bool, depth int) {
	if depth > 8 {
		return
	}
	if _, err := os.Stat(filepath.Join(p, "partition")); err == nil {
		p = filepath.Dir(p)
	}
	name := filepath.Base(p)
	if slaves, err := os.ReadDir(filepath.Join(p, "slaves")); err == nil && len(slaves) > 0 {
		for _, s := range slaves {
			if q, err := filepath.EvalSymlinks("/sys/class/block/" + s.Name()); err == nil {
				diskNamesForSysPath(q, out, depth+1)
			}
		}
		return
	}
	out[name] = true
}

// protectedDisks: the disks behind the system mounts, the running Parted
// Magic's boot device, and the disk holding the source.
func protectedDisks(sourcePath string) map[string]bool {
	out := map[string]bool{}
	for _, mp := range []string{"/", "/boot", "/home", "/usr", "/var", "/srv"} {
		var st unix.Stat_t
		if err := unix.Stat(mp, &st); err == nil {
			diskNamesForDevno(uint64(st.Dev), out)
		}
	}
	if dev := sysRead("/run/pmagic_device"); dev != "" {
		var st unix.Stat_t
		if err := unix.Stat(dev, &st); err == nil {
			diskNamesForDevno(uint64(st.Rdev), out)
		}
	}
	if sourcePath != "" {
		var st unix.Stat_t
		if err := unix.Stat(sourcePath, &st); err == nil {
			diskNamesForDevno(uint64(st.Dev), out)
		}
	}
	return out
}

func usbSerialFor(dev string) string {
	ents, _ := os.ReadDir("/dev/disk/by-id")
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, "usb-") || strings.Contains(n, "-part") {
			continue
		}
		if r, err := filepath.EvalSymlinks("/dev/disk/by-id/" + n); err == nil && r == dev {
			return strings.TrimSuffix(strings.TrimPrefix(n, "usb-"), "-0:0")
		}
	}
	return ""
}

func listDevices(all, fixed bool, sourcePath string) ([]Device, error) {
	prot := protectedDisks(sourcePath)
	ents, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil, err
	}
	var out []Device
	for _, e := range ents {
		n := e.Name()
		skip := false
		for _, pre := range []string{"loop", "ram", "dm-", "md", "sr", "zram", "nbd", "fd"} {
			if strings.HasPrefix(n, pre) {
				skip = true
			}
		}
		if strings.HasPrefix(n, "loop") && all {
			if _, err := os.Stat("/sys/block/" + n + "/loop/backing_file"); err == nil {
				skip = false // a file-backed loop device: usable for testing with --allow-non-usb
			}
		}
		if skip || prot[n] {
			continue
		}
		sb := "/sys/block/" + n
		real, _ := filepath.EvalSymlinks(sb + "/device")
		d := Device{Path: "/dev/" + n, USB: strings.Contains(real, "/usb")}
		if !all && !d.USB {
			continue
		}
		sz, _ := strconv.ParseInt(sysRead(sb+"/size"), 10, 64)
		d.Size = sz * 512
		d.Removable = sysRead(sb+"/removable") == "1"
		d.ReadOnly = sysRead(sb+"/ro") == "1"
		d.SectorSize, _ = strconv.Atoi(sysRead(sb + "/queue/logical_block_size"))
		d.Vendor = sysRead(sb + "/device/vendor")
		d.Model = strings.TrimSpace(d.Vendor + " " + sysRead(sb+"/device/model"))
		d.Serial = usbSerialFor(d.Path)
		if !fixed && d.USB && !d.Removable {
			continue
		}
		// A non-USB disk with anything mounted is somebody's data disk, not a target.
		if !d.USB && diskHasMounts(n) {
			continue
		}
		if d.Size == 0 {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func diskHasMounts(name string) bool {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return true
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		for i, x := range fs {
			if x == "-" && i+2 < len(fs) {
				if strings.HasPrefix(fs[i+2], "/dev/"+name) {
					return true
				}
				break
			}
		}
	}
	return false
}

func unmountPartitionsOf(dev string) error {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	defer f.Close()
	type mnt struct{ src, mp string }
	var mnts []mnt
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		for i, x := range fs {
			if x == "-" && i+2 < len(fs) {
				src := fs[i+2]
				if src == dev || strings.HasPrefix(src, dev) {
					mnts = append(mnts, mnt{src, strings.ReplaceAll(fs[4], "\\040", " ")})
				}
				break
			}
		}
	}
	udisks, _ := exec.LookPath("udisksctl")
	for _, m := range mnts {
		if udisks != "" {
			// Always through udisks when it is present. A raw umount2 behind its
			// back leaves its mounted-fs record in place, and its cleanup then
			// deletes the mount point directory under the NEXT mount of the
			// same name, which is the "No such file or directory" Thunar shows.
			out, err := exec.Command(udisks, "unmount", "--no-user-interaction", "-b", m.src).CombinedOutput()
			if err != nil {
				msg := strings.TrimSpace(string(out))
				return fmt.Errorf("could not unmount %s (%s): %s. Close programs and windows using it and try again", m.mp, m.src, msg)
			}
			emit("log", map[string]any{"phase": "prepare", "message": "unmounted " + m.mp + " through udisks"})
			continue
		}
		if err := unix.Unmount(m.mp, 0); err != nil {
			return fmt.Errorf("could not unmount %s: %v (it is in use; close programs and windows using it)", m.mp, err)
		}
		emit("log", map[string]any{"phase": "prepare", "message": "unmounted " + m.mp})
	}
	return nil
}

type devTarget struct {
	f    *os.File
	size int64
}

func (t *devTarget) ReadAt(p []byte, off int64) (int, error)  { return t.f.ReadAt(p, off) }
func (t *devTarget) WriteAt(p []byte, off int64) (int, error) { return t.f.WriteAt(p, off) }
func (t *devTarget) Size() int64                              { return t.size }
func (t *devTarget) Checkpoint() error                        { return unix.Fdatasync(int(t.f.Fd())) }
func (t *devTarget) Sync() error {
	if err := t.f.Sync(); err != nil {
		return err
	}
	_, _, e := unix.Syscall(unix.SYS_IOCTL, t.f.Fd(), unix.BLKFLSBUF, 0)
	if e != 0 {
		return e
	}
	return nil
}
func (t *devTarget) Close() error   { return t.f.Close() }
func (t *devTarget) File() *os.File { return t.f }
func (t *devTarget) IsDevice() bool { return true }

func newDevTarget(f *os.File) (*devTarget, error) {
	sz, err := unix.IoctlGetInt(int(f.Fd()), unix.BLKGETSIZE64)
	if err != nil {
		return nil, err
	}
	return &devTarget{f: f, size: int64(sz)}, nil
}

// directReadTarget: O_DIRECT reads through the block cache, so verification
// reads the stick, not the page cache that the write just filled.
type directReadTarget struct {
	f    *os.File
	size int64
	c    *blockCache
}

func (t *directReadTarget) rawRead(b []byte, off int64) error {
	n, err := t.f.ReadAt(b, off)
	if err != nil {
		return err
	}
	if n != len(b) {
		return fmt.Errorf("short read at %d", off)
	}
	return nil
}
func (t *directReadTarget) rawWrite(b []byte, off int64) error      { return errors.New("read-only target") }
func (t *directReadTarget) ReadAt(p []byte, off int64) (int, error) { return t.c.ReadAt(p, off) }
func (t *directReadTarget) WriteAt(p []byte, off int64) (int, error) {
	return 0, errors.New("read-only target")
}
func (t *directReadTarget) Size() int64          { return t.size }
func (t *directReadTarget) Checkpoint() error    { return nil }
func (t *directReadTarget) Sync() error          { return nil }
func (t *directReadTarget) Close() error         { return t.f.Close() }
func (t *directReadTarget) File() *os.File       { return t.f }
func (t *directReadTarget) IsDevice() bool       { return true }
func (t *directReadTarget) SetCacheBlocks(n int) { t.c.SetCacheBlocks(n) }

// openDevice applies the safety checks of the design map (section 7), unmounts
// the device's partitions, and opens it O_EXCL so nothing can mount it while
// the write runs.
func openDevice(path, expect string, allowNonUSB, allowFixed bool, sourcePath string) (Target, *Device, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, nil, err
	}
	devs, err := listDevices(allowNonUSB, allowFixed, sourcePath)
	if err != nil {
		return nil, nil, err
	}
	var d *Device
	for i := range devs {
		if devs[i].Path == real {
			d = &devs[i]
		}
	}
	if d == nil {
		if prot := protectedDisks(sourcePath); prot[filepath.Base(real)] {
			return nil, nil, errors.New("refused: " + real + " holds this system, the running Parted Magic, or the source ISO")
		}
		return nil, nil, errors.New("refused: " + real + " is not an eligible USB drive (use --allow-non-usb or --allow-fixed if you mean it)")
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
	if err := unmountPartitionsOf(real); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(real, os.O_RDWR|unix.O_EXCL, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s exclusively: %w (still mounted or in use?)", real, err)
	}
	t, err := newDevTarget(f)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return t, d, nil
}

func rereadPartitions(path string) error {
	// partprobe re-reads the table and, when the partition changed, emits a
	// remove then add uevent for it. That is what makes the desktop's volume
	// monitor (gvfs-udisks2) drop the old volume object and its stale mount
	// point and pick up the fresh one; a bare BLKRRPART can emit only a
	// "change", which leaves the ghost mount point Thunar then fails on.
	if pp, err := exec.LookPath("partprobe"); err == nil {
		if out, err := exec.Command(pp, path).CombinedOutput(); err != nil {
			emit("log", map[string]any{"phase": "finish", "message": "partprobe: " + strings.TrimSpace(string(out))})
		}
	} else {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err == nil {
			unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.BLKRRPART, 0)
			f.Close()
		}
	}
	ua, err := exec.LookPath("udevadm")
	if err != nil {
		return nil
	}
	exec.Command(ua, "settle", "-t", "10").Run()
	// partprobe's remove and add for the partition arrive about 60 ms apart
	// and gvfs-udisks2-volume-monitor races them: it builds its volume object
	// before udisks has finished probing the new filesystem, and every mount
	// from the desktop then fails with "No object for D-Bus interface" until
	// the drive is unplugged. A second remove/add with a pause between, the
	// software equivalent of a replug, lets it rebuild the object. Verified
	// against gvfs on a SanDisk on 2026-09-13.
	base := filepath.Base(path)
	parts, _ := filepath.Glob("/sys/block/" + base + "/" + base + "*")
	for _, p := range parts {
		exec.Command(ua, "trigger", "--action=remove", "/dev/"+filepath.Base(p)).Run()
	}
	time.Sleep(1200 * time.Millisecond)
	for _, p := range parts {
		exec.Command(ua, "trigger", "--action=add", "/dev/"+filepath.Base(p)).Run()
	}
	exec.Command(ua, "settle", "-t", "10").Run()
	return nil
}

// isDevicePath: a block device node.
func isDevicePath(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode()&os.ModeDevice != 0
}

// openReadTarget opens a block device or image read-only for verification.
func openReadTarget(path string) (Target, error) {
	if !isDevicePath(path) {
		return openFileTargetRO(path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_DIRECT, 0)
	if err != nil {
		return nil, err
	}
	sz, err := unix.IoctlGetInt(int(f.Fd()), unix.BLKGETSIZE64)
	if err != nil {
		f.Close()
		return nil, err
	}
	t := &directReadTarget{f: f, size: int64(sz)}
	t.c = &blockCache{dev: t, size: t.size}
	return t, nil
}
