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
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/diskfs/go-diskfs/filesystem/fat32"
)

const manifestPath = ".disk/installer-manifest.txt"

func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "T"):
		mult, s = 1<<40, s[:len(s)-1]
	case strings.HasSuffix(s, "G"):
		mult, s = 1<<30, s[:len(s)-1]
	case strings.HasSuffix(s, "M"):
		mult, s = 1<<20, s[:len(s)-1]
	case strings.HasSuffix(s, "K"):
		mult, s = 1<<10, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return n * mult, nil
}

// progressWriter reports per-file and overall progress every 8 MiB and at
// the end of each file: {"event":"progress","phase":..,"path":..,"file_done":..,
// "file_total":..,"done":..,"total":..}. A {"event":"file",...} precedes each file.
type progressWriter struct {
	w          io.Writer
	phase      string
	path       string
	fileTotal  int64
	fileDone   int64
	done       *int64
	total      int64
	lastRep    int64
	checkpoint func() error // called every 32 MiB of the overall count
	lastCP     int64
}

func (p *progressWriter) report() {
	// done/total span the entire operation (copy + verify); file_* is the current file.
	emit("progress", map[string]any{"phase": p.phase, "path": p.path, "file_done": p.fileDone, "file_total": p.fileTotal, "done": *p.done, "total": p.total})
}

func (p *progressWriter) Write(b []byte) (int, error) {
	if err := checkCanceled(); err != nil {
		return 0, err
	}
	n, err := p.w.Write(b)
	*p.done += int64(n)
	p.fileDone += int64(n)
	if *p.done-p.lastRep >= 8<<20 {
		p.lastRep = *p.done
		p.report()
	}
	if p.checkpoint != nil && *p.done-p.lastCP >= 32<<20 {
		p.lastCP = *p.done
		if cerr := p.checkpoint(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return n, err
}

func startFile(phase, path string, size int64) {
	emit("file", map[string]any{"phase": phase, "path": path, "size": size})
}

func cmdInstall(args []string) {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	src := fs.String("source", "", "ISO file or extracted tree")
	tgt := fs.String("target", "", "block device or image file")
	confirm := fs.Bool("confirm-erase", false, "required for a block device")
	expect := fs.String("expect", "", "model|serial|bytes the device must still report")
	sizeStr := fs.String("size", "", "image size for a file target, e.g. 3G (default: what the source needs)")
	labelStr := fs.String("label", "PMAGIC", "FAT volume label, 11 characters max")
	volIDStr := fs.String("volume-id", "", "FAT volume id, 8 hex digits (default: derived)")
	noVerify := fs.Bool("no-verify", false, "skip the read-back verification")
	noBIOS := fs.Bool("no-bios", false, "UEFI-only stick: no syslinux payload")
	allowMismatch := fs.Bool("allow-module-mismatch", false, "install the BIOS payload even if the ISO's modules are from another syslinux build")
	allowNonUSB := fs.Bool("allow-non-usb", false, "permit a non-USB target device")
	allowFixed := fs.Bool("allow-fixed", false, "permit a USB device that reports fixed media")
	cancelOnEOF := fs.Bool("cancel-on-eof", false, "treat end of stdin as a cancel request (front ends)")
	fs.BoolVar(&jsonOut, "json", false, "JSON Lines output")
	fs.Parse(args)
	startCancelWatcher(*cancelOnEOF)
	if *src == "" || *tgt == "" {
		usage()
	}
	label, err := normLabel(*labelStr)
	if err != nil {
		fail(2, "args", err.Error())
	}
	bios := !*noBIOS

	emit("phase", map[string]any{"phase": "source"})
	s, err := openSource(*src)
	if err != nil {
		fail(3, "source", err.Error())
	}
	defer s.Close()
	if err := s.validate(); err != nil {
		fail(3, "source", err.Error())
	}
	if bios && !s.ModuleMatch && !*allowMismatch {
		fail(3, "source", "this ISO's BIOS boot modules (boot/syslinux/ldlinux.c32) are from a different syslinux build than this installer's payload; update the installer, or pass --no-bios for a UEFI-only stick")
	}

	emit("phase", map[string]any{"phase": "prepare"})
	var t Target
	var dev *Device
	if isDevicePath(*tgt) {
		if !*confirm {
			fail(3, "prepare", "refusing to erase a device without --confirm-erase")
		}
		t, dev, err = openDevice(*tgt, *expect, *allowNonUSB, *allowFixed, *src)
		if err != nil {
			fail(3, "prepare", err.Error())
		}
	} else {
		var size int64
		if *sizeStr != "" {
			if size, err = parseSize(*sizeStr); err != nil {
				fail(2, "args", "bad --size: "+err.Error())
			}
		} else {
			size = (requiredBytes(s.entries, 32*1024) + 1<<20 - 1) &^ (1<<20 - 1)
		}
		if t, err = createFileTarget(*tgt, size); err != nil {
			fail(4, "prepare", err.Error())
		}
	}
	defer t.Close()

	g, err := computeGeometry(uint64(t.Size() / 512))
	if err != nil {
		fail(3, "prepare", err.Error())
	}
	if cs, ok := t.(cacheSizer); ok {
		cs.SetCacheBlocks(cacheBlocksFor(int64(g.fatlen) * 512))
	}
	need := requiredBytes(s.entries, g.clusterBytes())
	if have := int64(g.clusters) * g.clusterBytes(); have < need {
		fail(3, "prepare", fmt.Sprintf("drive too small: needs %d bytes, has %d", need, have))
	}
	mbrCode, err := s.readAll("boot/syslinux/mbr.bin")
	if err != nil || len(mbrCode) != 440 {
		fail(3, "source", "boot/syslinux/mbr.bin unreadable or not 440 bytes")
	}
	var volID uint32
	if *volIDStr != "" {
		v, err := strconv.ParseUint(*volIDStr, 16, 32)
		if err != nil {
			fail(2, "args", "bad --volume-id")
		}
		volID = uint32(v)
	} else {
		// Random, like mkfs.fat: a repeated volume id makes udisks reuse the
		// same mount point path for the new volume while it is still tearing
		// down the old one, and Thunar then fails to mount the fresh stick.
		var rb [4]byte
		if _, err := rand.Read(rb[:]); err != nil {
			fail(4, "prepare", err.Error())
		}
		volID = binary.LittleEndian.Uint32(rb[:])
		if volID == 0 {
			volID = 1
		}
	}
	// Old table, old VBRs, stale GPT headers at both ends.
	if err := zeroRange(t, 0, 1<<20); err != nil {
		fail(4, "prepare", err.Error())
	}
	if t.Size() > 2<<20 {
		if err := zeroRange(t, t.Size()-(1<<20), 1<<20); err != nil {
			fail(4, "prepare", err.Error())
		}
	}

	emit("phase", map[string]any{"phase": "format"})
	if err := writeEmptyFAT32(t, g, label, volID); err != nil {
		fail(4, "format", err.Error())
	}
	if err := t.Sync(); err != nil {
		fail(4, "format", err.Error())
	}

	// Overall progress spans the two long phases: writing every file, then
	// reading every file back to verify. Small phases (format, bootloader,
	// flush) do not move it. opDone is shared so the bar never restarts.
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
	var opDone int64

	emit("phase", map[string]any{"phase": "copy"})
	hashes, err := populate(t, g, s, bios, &opDone, opTotal)
	if errors.Is(err, errCanceled) {
		emit("canceled", map[string]any{"phase": "copy", "message": "canceled; the drive has no partition table"})
		os.Exit(6)
	}
	if err != nil {
		fail(4, "copy", err.Error())
	}
	if err := t.Sync(); err != nil {
		fail(4, "copy", err.Error())
	}
	if f, err := openFAT(t, g.partByte()); err != nil {
		fail(4, "copy", err.Error())
	} else if err := repairTree(f); err != nil {
		fail(4, "copy", "repair pass: "+err.Error())
	}
	if _, _, err := fsinfoRecount(t, g); err != nil {
		fail(4, "copy", err.Error())
	}

	if bios {
		emit("phase", map[string]any{"phase": "bootloader"})
		f, err := openFAT(t, g.partByte())
		if err != nil {
			fail(4, "bootloader", err.Error())
		}
		if err := installSyslinux(f, s.PayloadSys, s.PayloadBss); err != nil {
			fail(4, "bootloader", err.Error())
		}
	}
	emit("phase", map[string]any{"phase": "flush"})
	if err := t.Sync(); err != nil {
		fail(4, "flush", err.Error())
	}

	if !*noVerify {
		emit("phase", map[string]any{"phase": "verify"})
		if err := verifyTarget(t, g.partByte(), hashes, bios, &opDone, opTotal); errors.Is(err, errCanceled) {
			emit("canceled", map[string]any{"phase": "verify", "message": "canceled; the drive has no partition table"})
			os.Exit(6)
		} else if err != nil {
			fail(5, "verify", err.Error()+" (the drive has no partition table and is not bootable; run the installer again)")
		}
	}

	emit("phase", map[string]any{"phase": "finish"})
	sig := volID ^ 0xA5A5A5A5
	if sig == 0 {
		sig = 1
	}
	sec0 := mbrSector(mbrCode, g, sig)
	if _, err := t.WriteAt(sec0, 0); err != nil {
		fail(4, "finish", err.Error())
	}
	if err := t.Sync(); err != nil {
		fail(4, "finish", err.Error())
	}
	back := make([]byte, 512)
	if _, err := t.ReadAt(back, 0); err != nil || !bytes.Equal(back, sec0) {
		fail(4, "finish", "partition table read-back mismatch")
	}
	if dev != nil {
		t.Close()
		if err := rereadPartitions(dev.Path); err != nil {
			emit("log", map[string]any{"phase": "finish", "message": "partition re-read failed: " + err.Error() + " (unplug and reconnect the drive)"})
		}
	}
	emit("done", map[string]any{"phase": "finish", "message": fmt.Sprintf("Parted Magic %s written to %s", s.Version, *tgt), "verified": !*noVerify, "bios": bios})
}

// populate writes the tree through go-diskfs opened at the partition offset:
// directories, every file but the SQFS, ldlinux.sys, the SQFS last (one long
// sequential run), then the manifest. Returns the SHA-256 of every copied file.
func populate(t Target, g geom, s *Source, bios bool, opDone *int64, opTotal int64) (map[string]string, error) {
	fsys, err := fat32.Read(&targetStorage{t: t}, int64(g.psize)*512, g.partByte(), 512)
	if err != nil {
		return nil, fmt.Errorf("open FAT32 for writing: %w", err)
	}
	hashes := map[string]string{}
	buf := make([]byte, 4<<20)
	copyOne := func(e entry) error {
		r, err := s.fs.open(e.p)
		if err != nil {
			return fmt.Errorf("open %s: %w", e.p, err)
		}
		defer r.Close()
		w, err := fsys.OpenFile(e.p, os.O_CREATE|os.O_RDWR|os.O_TRUNC)
		if err != nil {
			return fmt.Errorf("create %s: %w", e.p, err)
		}
		h := sha256.New()
		startFile("copy", e.p, e.size)
		pw := &progressWriter{w: io.MultiWriter(w, h), phase: "copy", path: e.p, fileTotal: e.size, done: opDone, total: opTotal, lastRep: *opDone, checkpoint: t.Checkpoint, lastCP: *opDone}
		n, err := io.CopyBuffer(pw, r, buf)
		pw.report()
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("copy %s: %w", e.p, err)
		}
		if n != e.size {
			return fmt.Errorf("copy %s: short copy %d of %d", e.p, n, e.size)
		}
		hashes[e.p] = hex.EncodeToString(h.Sum(nil))
		return nil
	}
	for _, e := range s.entries {
		// ldlinux.sys is generated for this stick, never copied from the source.
		if skipped(e.p) || e.p == s.SQFS || e.p == ldlinuxPath {
			continue
		}
		if e.dir {
			if err := fsys.Mkdir(e.p); err != nil {
				return nil, fmt.Errorf("mkdir %s: %w", e.p, err)
			}
			continue
		}
		if err := copyOne(e); err != nil {
			return nil, err
		}
	}
	if bios {
		w, err := fsys.OpenFile(ldlinuxPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC)
		if err != nil {
			return nil, fmt.Errorf("create %s: %w", ldlinuxPath, err)
		}
		if _, err := w.Write(ldlinuxSysContent(s.PayloadSys)); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
	}
	if err := copyOne(*s.byPath[s.SQFS]); err != nil {
		return nil, err
	}
	// manifest
	var mb strings.Builder
	fmt.Fprintf(&mb, "pmusb-manifest 1\ninstaller %s\npayload %s\nlabel %s\nversion %s\nbuild %s\nsqfs %s\nbios %v\nfiles:\n",
		version, s.payloadDesc(), s.Label, s.Version, s.Build, s.SQFS, bios)
	keys := make([]string, 0, len(hashes))
	for k := range hashes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&mb, "%s  %s\n", hashes[k], k)
	}
	w, err := fsys.OpenFile(manifestPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC)
	if err != nil {
		return nil, fmt.Errorf("create manifest: %w", err)
	}
	if _, err := w.Write([]byte(mb.String())); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if err := fsys.Close(); err != nil {
		return nil, err
	}
	return hashes, nil
}
