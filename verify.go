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

package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

// verifyTarget reads the stick back through the core's own FAT reader and
// checks structure, every file's hash, and the BIOS boot loader.
func verifyTarget(t Target, pb int64, hashes map[string]string, bi bootInfo, opDone *int64, opTotal int64) error {
	f, err := openFAT(t, pb)
	if err != nil {
		return err
	}
	if cs, ok := t.(cacheSizer); ok {
		cs.SetCacheBlocks(cacheBlocksFor(int64(f.fatsz) * 512))
	}
	s0, err := f.readSectors(0, 1)
	if err != nil {
		return err
	}
	s6, _ := f.readSectors(6, 1)
	// Byte 0x41 (BS_Reserved1) is the "volume in use" flag that both Windows
	// and the Linux vfat driver set in the primary boot sector while the
	// volume is mounted read-write, and clear on a clean unmount. It carries
	// no boot or data content, so it is excluded from the comparison.
	if len(s0) > 0x41 && len(s6) > 0x41 && s0[0x41] != s6[0x41] {
		emit("log", map[string]any{"phase": "verify", "message": fmt.Sprintf("volume in-use flag is %#x in the boot sector and %#x in the backup (the volume is mounted, or was not cleanly unmounted)", s0[0x41], s6[0x41])})
		s6 = append([]byte(nil), s6...)
		s6[0x41] = s0[0x41]
	}
	if !bytes.Equal(s0, s6) {
		return fmt.Errorf("boot sector and its backup differ")
	}
	var free uint32
	for c := uint32(2); c < f.clusters+2; c++ {
		if f.next(c) == 0 {
			free++
		}
	}
	for _, sec := range []uint32{1, 7} {
		fsi, err := f.readSectors(sec, 1)
		if err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(fsi[0:]) != 0x41615252 || binary.LittleEndian.Uint32(fsi[484:]) != 0x61417272 || binary.LittleEndian.Uint32(fsi[508:]) != 0xAA550000 {
			return fmt.Errorf("FSInfo sector %d has bad signatures", sec)
		}
		// The FSInfo free count is advisory (fatgen103: "may be incorrect");
		// Windows updates it lazily once it has mounted the volume, so a stale
		// value after a mount is normal and no boot or data depends on it.
		if sec == 1 && binary.LittleEndian.Uint32(fsi[488:]) != free {
			emit("log", map[string]any{"phase": "verify", "message": fmt.Sprintf("FSInfo free count %d differs from the FAT's %d (advisory field, updated lazily by Windows)", binary.LittleEndian.Uint32(fsi[488:]), free)})
		}
	}
	fat2, err := f.readSectors(f.reserved+f.fatsz, int(f.fatsz))
	if err != nil {
		return err
	}
	if !bytes.Equal(fat2, f.fat) {
		return fmt.Errorf("the two FAT copies differ")
	}
	used := make([]uint64, (f.clusters+2+63)/64)
	mark := func(c uint32) bool {
		w, b := c/64, uint64(1)<<(c%64)
		if used[w]&b != 0 {
			return false
		}
		used[w] |= b
		return true
	}
	paths := make([]string, 0, len(hashes))
	for p := range hashes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	cb := int64(f.spc) * 512
	ents := map[string]*dirent{}
	var sum int64
	for _, p := range paths {
		d, err := f.lookup(p)
		if err != nil {
			return err
		}
		ents[p] = d
		sum += int64(d.Size)
	}
	if opTotal == 0 {
		opTotal = sum
	}
	for _, p := range paths {
		if err := checkCanceled(); err != nil {
			return err
		}
		d := ents[p]
		if d.Attr&0x10 != 0 {
			return fmt.Errorf("%s is a directory on the target", p)
		}
		ch, err := f.fileClusters(d)
		if err != nil {
			return fmt.Errorf("%s: %v", p, err)
		}
		if want := (int64(d.Size) + cb - 1) / cb; int64(len(ch)) != want {
			return fmt.Errorf("%s: chain has %d clusters, size needs %d", p, len(ch), want)
		}
		for _, c := range ch {
			if !mark(c) {
				return fmt.Errorf("%s: cluster %d is used twice", p, c)
			}
		}
		h := sha256.New()
		startFile("verify", p, int64(d.Size))
		pw := &progressWriter{w: h, phase: "verify", path: p, fileTotal: int64(d.Size), done: opDone, total: opTotal, lastRep: *opDone}
		if _, err := f.readFile(d, pw); err != nil {
			return fmt.Errorf("%s: %v", p, err)
		}
		pw.report()
		if got := hex.EncodeToString(h.Sum(nil)); got != hashes[p] {
			return fmt.Errorf("verification failed at %s: content differs from the source", p)
		}
	}
	switch bi.Loader {
	case loaderSyslinux:
		return verifySyslinux(f)
	case loaderGRUB:
		return verifyGRUB(t, bi.CoreHash, bi.CoreSectors)
	}
	return nil
}

func verifySyslinux(f *fatFS) error {
	d, err := f.lookup(ldlinuxPath)
	if err != nil {
		return err
	}
	if d.Attr&0x07 != 0x07 {
		return fmt.Errorf("ldlinux.sys attributes are %#x, expected read-only+hidden+system", d.Attr)
	}
	secs, err := f.fileSectors(d)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if _, err := f.readFile(d, &buf); err != nil {
		return err
	}
	img := buf.Bytes()
	off := 0
	for off+4 <= len(img) && binary.LittleEndian.Uint32(img[off:]) != ldlinuxMagic {
		off += 4
	}
	if off+24 > len(img) {
		return fmt.Errorf("ldlinux.sys: magic not found")
	}
	dw := int(binary.LittleEndian.Uint32(img[off+12:]))
	if dw*4 > len(img) {
		return fmt.Errorf("ldlinux.sys: dword count out of range")
	}
	var sum uint32
	for i := 0; i < dw; i++ {
		sum += binary.LittleEndian.Uint32(img[4*i:])
	}
	if sum != ldlinuxMagic {
		return fmt.Errorf("ldlinux.sys: checksum mismatch")
	}
	epao := int(binary.LittleEndian.Uint16(img[off+22:]))
	secptroffset := int(binary.LittleEndian.Uint16(img[epao+10:]))
	secptrcnt := int(binary.LittleEndian.Uint16(img[epao+12:]))
	sect1ptr0 := int(binary.LittleEndian.Uint16(img[epao+14:]))
	var ext []uint32
	for k := 0; k < secptrcnt; k++ {
		lba := binary.LittleEndian.Uint64(img[secptroffset+10*k:])
		n := binary.LittleEndian.Uint16(img[secptroffset+10*k+8:])
		if n == 0 {
			break
		}
		for i := uint16(0); i < n; i++ {
			ext = append(ext, uint32(lba)+uint32(i))
		}
	}
	dataSecs := int(binary.LittleEndian.Uint16(img[off+8:]))
	if len(secs) < dataSecs+2 || dataSecs < 2 {
		return fmt.Errorf("ldlinux.sys: sector count mismatch")
	}
	want := secs[1:dataSecs]
	if len(ext) != len(want) {
		return fmt.Errorf("ldlinux.sys: extent table covers %d sectors, file has %d", len(ext), len(want))
	}
	for i := range want {
		if ext[i] != want[i] {
			return fmt.Errorf("ldlinux.sys: extent %d points at sector %d, file is at %d", i, ext[i], want[i])
		}
	}
	vbr, err := f.readSectors(0, 1)
	if err != nil {
		return err
	}
	if binary.LittleEndian.Uint32(vbr[sect1ptr0:]) != secs[0] {
		return fmt.Errorf("boot sector points at sector %d, ldlinux.sys starts at %d", binary.LittleEndian.Uint32(vbr[sect1ptr0:]), secs[0])
	}
	return nil
}

func cmdVerify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	tgt := fs.String("target", "", "device or image file")
	fs.BoolVar(&jsonOut, "json", false, "JSON Lines output")
	fs.Parse(args)
	if *tgt == "" {
		usage()
	}
	t, err := openReadTarget(*tgt)
	if err != nil {
		fail(4, "verify", err.Error())
	}
	defer t.Close()
	sec0 := make([]byte, 512)
	if _, err := t.ReadAt(sec0, 0); err != nil {
		fail(4, "verify", err.Error())
	}
	pb := int64(2048) * 512
	if sec0[510] == 0x55 && sec0[511] == 0xAA && (sec0[450] == 0x0C || sec0[450] == 0x0B) {
		pb = int64(binary.LittleEndian.Uint32(sec0[454:])) * 512
	} else {
		emit("log", map[string]any{"phase": "verify", "message": "no partition table; assuming the filesystem starts at 1 MiB"})
	}
	f, err := openFAT(t, pb)
	if err != nil {
		fail(5, "verify", err.Error())
	}
	d, err := f.lookup(manifestPath)
	if err != nil {
		fail(5, "verify", "no installer manifest on the target: "+err.Error())
	}
	var buf bytes.Buffer
	if _, err := f.readFile(d, &buf); err != nil {
		fail(5, "verify", err.Error())
	}
	hashes := map[string]string{}
	var bi bootInfo
	inFiles := false
	sc := bufio.NewScanner(&buf)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case inFiles:
			if i := strings.Index(line, "  "); i == 64 {
				hashes[line[i+2:]] = line[:64]
			}
		case line == "files:":
			inFiles = true
		default:
			parseBootLine(line, &bi)
		}
	}
	var opDone int64
	if err := verifyTarget(t, pb, hashes, bi, &opDone, 0); err != nil {
		fail(5, "verify", err.Error())
	}
	if bi.Loader == loaderGRUB {
		if err := verifyGRUBMBR(sec0); err != nil {
			fail(5, "verify", err.Error())
		}
	}
	emit("done", map[string]any{"phase": "verify", "message": fmt.Sprintf("%d files verified, bios=%v, loader=%s", len(hashes), bi.Loader != loaderNone, bi.Loader)})
	os.Exit(0)
}
