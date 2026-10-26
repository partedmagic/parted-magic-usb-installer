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
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// Upgrade in place (design section 5): keep the saved session, bundles and the
// user's own files, replace Parted Magic. The copy goes through the kernel's
// vfat driver on a temporary mount; the boot loader step runs raw afterward.
// A stick that had a BIOS loader gets the new ISO's: an older syslinux stick
// upgraded to a GRUB ISO becomes a GRUB stick.
func cmdUpgrade(args []string) {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	src := fs.String("source", "", "ISO file or extracted tree")
	tgt := fs.String("target", "", "block device or image file holding a Parted Magic stick")
	confirm := fs.Bool("confirm-upgrade", false, "required")
	deleteSession := fs.Bool("delete-session", false, "remove a saved session made on another version")
	noVerify := fs.Bool("no-verify", false, "skip the read-back verification")
	allowMismatch := fs.Bool("allow-module-mismatch", false, "install the BIOS payload even if the ISO's modules are from another syslinux build")
	fs.BoolVar(&jsonOut, "json", false, "JSON Lines output")
	fs.Parse(args)
	if *src == "" || *tgt == "" {
		usage()
	}
	if !*confirm {
		fail(3, "prepare", "refusing to change the drive without --confirm-upgrade")
	}
	emit("phase", map[string]any{"phase": "source"})
	s, err := openSource(*src)
	if err != nil {
		fail(3, "source", err.Error())
	}
	defer s.Close()
	if err := s.validate(); err != nil {
		fail(3, "source", err.Error())
	}
	// Locate the partition and read the old manifest through the core's own reader.
	emit("phase", map[string]any{"phase": "prepare"})
	ro, err := openFileTargetRO(*tgt)
	if err != nil {
		fail(4, "prepare", err.Error())
	}
	sec0 := make([]byte, 512)
	if _, err := ro.ReadAt(sec0, 0); err != nil {
		fail(4, "prepare", err.Error())
	}
	if sec0[510] != 0x55 || sec0[511] != 0xAA || (sec0[450] != 0x0C && sec0[450] != 0x0B) || sec0[462] != 0 {
		fail(3, "prepare", "not a Parted Magic stick made by this installer (expected one FAT32 partition)")
	}
	pb := int64(binary.LittleEndian.Uint32(sec0[454:])) * 512
	f, err := openFAT(ro, pb)
	if err != nil {
		fail(3, "prepare", err.Error())
	}
	// The installer's manifest marks a stick it made; /.disk/info is the
	// ISO's own and says different things on different ISOs.
	old, err := readFATFile(f, manifestPath)
	if err != nil {
		fail(3, "prepare", "no installer manifest on the drive; use install for a fresh copy")
	}
	oldFiles, oldVersion, oldBIOS := parseManifest(old)
	bios := oldBIOS && s.Loader != loaderNone
	if s.BootLabel != "" {
		vbr := make([]byte, 512)
		if _, err := ro.ReadAt(vbr, pb); err != nil {
			fail(4, "prepare", err.Error())
		}
		if have := strings.TrimRight(string(vbr[71:82]), " "); have != s.BootLabel {
			fail(3, "prepare", fmt.Sprintf("this ISO finds its stick by the volume label %s and the drive's is %s; use install for a fresh copy", s.BootLabel, have))
		}
	}
	if bios && !s.ModuleMatch && !*allowMismatch {
		fail(3, "source", "this ISO's BIOS boot modules are from a different syslinux build than this installer's payload; update the installer")
	}
	bi := s.bootInfo(bios)
	if bi.Loader == loaderGRUB && pb/512 < grubCoreLBA+int64(bi.CoreSectors) {
		fail(3, "prepare", "the partition starts too early for GRUB's core image; use install for a fresh copy")
	}
	sessionD, _ := f.lookup("pmagic/pmodules/099-saved-session.sqfm")
	if sessionD != nil && oldVersion != s.Version && !*deleteSession {
		emit("log", map[string]any{"phase": "prepare", "message": fmt.Sprintf("keeping a saved session made on %s; pass --delete-session to remove it", oldVersion)})
	}
	ro.Close()

	// Mount, copy, unmount.
	dev := *tgt
	mnt, err := os.MkdirTemp("", "pmusb-upgrade-")
	if err != nil {
		fail(4, "prepare", err.Error())
	}
	defer os.Remove(mnt)
	loopDev := ""
	part := dev
	if st, err := os.Stat(dev); err == nil && st.Mode()&os.ModeDevice != 0 {
		if err := unmountPartitionsOf(dev); err != nil {
			fail(3, "prepare", err.Error())
		}
		part = partitionNode(dev)
	} else {
		out, err := runCmd("losetup", "-f", "--show", "-P", dev)
		if err != nil {
			fail(4, "prepare", "losetup: "+err.Error())
		}
		loopDev = strings.TrimSpace(out)
		part = loopDev + "p1"
		defer runCmd("losetup", "-d", loopDev)
	}
	if err := unix.Mount(part, mnt, "vfat", 0, "shortname=mixed,utf8"); err != nil {
		fail(4, "prepare", fmt.Sprintf("mount %s: %v", part, err))
	}
	mounted := true
	um := func() {
		if mounted {
			unix.Unmount(mnt, 0)
			mounted = false
		}
	}
	defer um()

	emit("phase", map[string]any{"phase": "copy"})
	newSet := map[string]bool{}
	for _, e := range s.entries {
		if !skipped(e.p) {
			newSet[e.p] = true
		}
	}
	// Old Parted Magic files the new ISO does not carry go first (includes the old SQFS).
	for p := range oldFiles {
		if !newSet[p] {
			os.Remove(filepath.Join(mnt, filepath.FromSlash(p)))
		}
	}
	if sessionD != nil && oldVersion != s.Version && *deleteSession {
		os.Remove(filepath.Join(mnt, "pmagic/pmodules/099-saved-session.sqfm"))
	}
	if s.SQFS != "" {
		os.Remove(filepath.Join(mnt, filepath.FromSlash(s.SQFS)))
	}
	os.Remove(filepath.Join(mnt, filepath.FromSlash(ldlinuxPath)))
	hashes := map[string]string{}
	var copyTotal int64
	for _, e := range s.entries {
		if !e.dir && !skipped(e.p) && e.p != ldlinuxPath {
			copyTotal += e.size
		}
	}
	opTotal := copyTotal
	if !*noVerify {
		opTotal += copyTotal
	}
	var done int64
	if err := unix.Statfs(mnt, &unix.Statfs_t{}); err != nil {
		fail(4, "copy", err.Error())
	}
	var sfs unix.Statfs_t
	unix.Statfs(mnt, &sfs)
	if free := int64(sfs.Bavail) * int64(sfs.Bsize); free < copyTotal+64<<20 {
		fail(3, "copy", fmt.Sprintf("not enough free space on the drive: %d bytes free, %d needed", free, copyTotal+64<<20))
	}
	buf := make([]byte, 4<<20)
	copyOne := func(e entry) error {
		dst := filepath.Join(mnt, filepath.FromSlash(e.p))
		r, err := s.fs.open(e.p)
		if err != nil {
			return err
		}
		defer r.Close()
		w, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			return err
		}
		h := sha256.New()
		startFile("copy", e.p, e.size)
		pw := &progressWriter{w: io.MultiWriter(w, h), phase: "copy", path: e.p, fileTotal: e.size, done: &done, total: opTotal, lastRep: done,
			checkpoint: func() error { return unix.Fdatasync(int(w.Fd())) }, lastCP: done}
		n, err := io.CopyBuffer(pw, r, buf)
		pw.report()
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if n != e.size {
			return fmt.Errorf("short copy of %s", e.p)
		}
		hashes[e.p] = hex.EncodeToString(h.Sum(nil))
		return nil
	}
	for _, e := range s.entries {
		if skipped(e.p) || e.p == s.SQFS || e.p == ldlinuxPath {
			continue
		}
		if e.dir {
			os.MkdirAll(filepath.Join(mnt, filepath.FromSlash(e.p)), 0755)
			continue
		}
		if err := copyOne(e); err != nil {
			fail(4, "copy", err.Error())
		}
	}
	if bi.Loader == loaderSyslinux {
		if err := os.WriteFile(filepath.Join(mnt, filepath.FromSlash(ldlinuxPath)), ldlinuxSysContent(s.PayloadSys), 0644); err != nil {
			fail(4, "copy", err.Error())
		}
	}
	if s.SQFS != "" {
		if err := copyOne(*s.byPath[s.SQFS]); err != nil {
			fail(4, "copy", err.Error())
		}
	}
	var mb strings.Builder
	mb.WriteString(manifestHeader(s, bi))
	keys := make([]string, 0, len(hashes))
	for k := range hashes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&mb, "%s  %s\n", hashes[k], k)
	}
	if err := os.WriteFile(filepath.Join(mnt, filepath.FromSlash(manifestPath)), []byte(mb.String()), 0644); err != nil {
		fail(4, "copy", err.Error())
	}
	unix.Sync()
	um()

	// Raw part: boot loader, FSInfo, verify.
	t, err := openRWTarget(dev)
	if err != nil {
		fail(4, "bootloader", err.Error())
	}
	defer t.Close()
	switch bi.Loader {
	case loaderSyslinux:
		emit("phase", map[string]any{"phase": "bootloader"})
		f, err := openFAT(t, pb)
		if err != nil {
			fail(4, "bootloader", err.Error())
		}
		if err := installSyslinux(f, s.PayloadSys, s.PayloadBss); err != nil {
			fail(4, "bootloader", err.Error())
		}
		// A stick going back from GRUB to syslinux needs syslinux's MBR code.
		if code, err := s.readAll("boot/syslinux/mbr.bin"); err == nil && len(code) == 440 {
			sec0 := make([]byte, 512)
			if _, err := t.ReadAt(sec0, 0); err == nil && !bytes.Equal(sec0[:440], code) {
				copy(sec0, code)
				if _, err := t.WriteAt(sec0, 0); err != nil {
					fail(4, "bootloader", err.Error())
				}
			}
		}
	case loaderGRUB:
		emit("phase", map[string]any{"phase": "bootloader"})
		if err := installGRUB(t, s.GrubCore); err != nil {
			fail(4, "bootloader", err.Error())
		}
		if err := writeGRUBMBRCode(t, s.GrubBoot); err != nil {
			fail(4, "bootloader", err.Error())
		}
	}
	g, err := geometryFromVBR(t, pb)
	if err != nil {
		fail(4, "bootloader", err.Error())
	}
	if _, _, err := fsinfoRecount(t, g); err != nil {
		fail(4, "bootloader", err.Error())
	}
	if err := t.Sync(); err != nil {
		fail(4, "flush", err.Error())
	}
	if !*noVerify {
		emit("phase", map[string]any{"phase": "verify"})
		if err := verifyTarget(t, pb, hashes, bi, &done, opTotal); err != nil {
			fail(5, "verify", err.Error())
		}
	}
	to := s.Version
	if to == "" {
		to = s.name()
	}
	emit("done", map[string]any{"phase": "finish", "message": fmt.Sprintf("Parted Magic %s upgraded to %s on %s", oldVersion, to, *tgt), "verified": !*noVerify, "bios": bios, "loader": bi.Loader})
}

func readFATFile(f *fatFS, p string) ([]byte, error) {
	d, err := f.lookup(p)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if _, err := f.readFile(d, &b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func parseManifest(b []byte) (files map[string]string, version string, bios bool) {
	files = map[string]string{}
	inFiles := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case inFiles:
			if i := strings.Index(line, "  "); i == 64 {
				files[line[i+2:]] = line[:64]
			}
		case line == "files:":
			inFiles = true
		case strings.HasPrefix(line, "version "):
			version = strings.TrimSpace(line[8:])
		case strings.HasPrefix(line, "bios "):
			bios = strings.TrimSpace(line[5:]) == "true"
		}
	}
	return
}

func partitionNode(dev string) string {
	base := filepath.Base(dev)
	if base[len(base)-1] >= '0' && base[len(base)-1] <= '9' {
		return dev + "p1"
	}
	return dev + "1"
}

func runCmd(name string, args ...string) (string, error) {
	out, err := execCommand(name, args...)
	return out, err
}

// openRWTarget opens a device (O_EXCL) or an image file for the raw steps.
func openRWTarget(path string) (Target, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode()&os.ModeDevice != 0 {
		fd, err := os.OpenFile(path, os.O_RDWR|unix.O_EXCL, 0)
		if err != nil {
			return nil, err
		}
		t, err := newDevTarget(fd)
		if err != nil {
			fd.Close()
			return nil, err
		}
		return t, nil
	}
	fd, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return &fileTarget{f: fd, size: st.Size()}, nil
}

// geometryFromVBR rebuilds the geom fields fsinfoRecount needs from the BPB.
func geometryFromVBR(t Target, pb int64) (geom, error) {
	bs := make([]byte, 512)
	if _, err := t.ReadAt(bs, pb); err != nil {
		return geom{}, err
	}
	g := geom{pstart: uint32(pb / 512), spc: uint32(bs[13]), reserved: uint32(binary.LittleEndian.Uint16(bs[14:])),
		fatlen: binary.LittleEndian.Uint32(bs[36:]), psize: binary.LittleEndian.Uint32(bs[32:])}
	g.clusters = (g.psize - g.reserved - 2*g.fatlen) / g.spc
	return g, nil
}
