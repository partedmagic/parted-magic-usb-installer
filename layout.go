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
	"encoding/binary"
	"fmt"
	"strings"
)

// geom is the on-disk layout: one partition at LBA 2048, FAT32 sized the way
// mkfs.fat 4.2 sizes it (design section 2).
type geom struct {
	pstart, partSectors uint32 // MBR entry
	psize               uint32 // FAT32 total sectors (rounded to a track multiple)
	spc, reserved       uint32
	fatlen, clusters    uint32
	heads               uint32
}

func align(x, a uint32) uint32 { return (x + a - 1) &^ (a - 1) }

func computeGeometry(totalSectors uint64) (geom, error) {
	if totalSectors > 0xFFFFFFFF {
		return geom{}, fmt.Errorf("device has %d sectors; FAT32 is limited to 2 TiB", totalSectors)
	}
	if totalSectors < 2048+70000*2 {
		return geom{}, fmt.Errorf("device too small for FAT32")
	}
	g := geom{pstart: 2048}
	g.partSectors = uint32(totalSectors) - g.pstart
	psize := g.partSectors
	switch {
	case psize <= 16*63*1024:
		g.heads = 16
	case psize <= 32*63*1024:
		g.heads = 32
	case psize <= 64*63*1024:
		g.heads = 64
	case psize <= 128*63*1024:
		g.heads = 128
	default:
		g.heads = 255
	}
	psize = psize / 63 * 63 // mkfs.fat.c:792
	g.psize = psize
	switch {
	case psize > 32*1024*1024*2:
		g.spc = 64
	case psize > 16*1024*1024*2:
		g.spc = 32
	case psize > 8*1024*1024*2:
		g.spc = 16
	case psize > 260*1024*2:
		g.spc = 8
	default:
		g.spc = 1
	}
	for {
		g.reserved = align(32, g.spc)
		fatdata := psize - g.reserved
		clust := uint32((uint64(fatdata)*512 + 2*8) / (uint64(g.spc)*512 + 2*4))
		fl := ((clust+2)*4 + 511) / 512
		fl = align(fl, g.spc)
		clust = (fatdata - 2*fl) / g.spc
		maxclust := fl * 512 / 4
		if maxclust > 268435446 {
			maxclust = 268435446
		}
		if clust <= maxclust && clust >= 65525 {
			g.fatlen, g.clusters = fl, clust
			return g, nil
		}
		g.spc <<= 1
		if g.spc > 128 {
			return geom{}, fmt.Errorf("no valid FAT32 geometry for %d sectors", totalSectors)
		}
	}
}

func (g geom) clusterBytes() int64 { return int64(g.spc) * 512 }
func (g geom) partByte() int64     { return int64(g.pstart) * 512 }

// requiredBytes estimates the space the copied tree needs at a given cluster size.
func requiredBytes(entries []entry, clusterBytes int64) int64 {
	var n int64
	for _, e := range entries {
		if skipped(e.p) {
			continue
		}
		if e.dir {
			n += clusterBytes
			continue
		}
		n += (e.size + clusterBytes - 1) / clusterBytes * clusterBytes
	}
	return n + 64<<20 + 1<<20 + 60*1024 // margin, partition offset, ldlinux.sys and manifest
}

func normLabel(s string) (string, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		s = "PMAGIC"
	}
	if len(s) > 11 {
		return "", fmt.Errorf("label %q is longer than 11 characters", s)
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == ' ') {
			return "", fmt.Errorf("label %q contains a character FAT does not allow", s)
		}
	}
	return s, nil
}

func chs(lba uint32) (byte, byte, byte) {
	c := lba / (255 * 63)
	h := (lba / 63) % 255
	s := lba%63 + 1
	if c > 1023 {
		return 0xFE, 0xFF, 0xFF
	}
	return byte(h), byte(s) | byte((c>>8)<<6), byte(c)
}

// mbrSector builds sector 0: boot code, disk signature, one active type-0x0C entry.
func mbrSector(code []byte, g geom, sig uint32) []byte {
	s := make([]byte, 512)
	copy(s, code[:440])
	binary.LittleEndian.PutUint32(s[440:], sig)
	e := s[446:462]
	e[0] = 0x80
	e[1], e[2], e[3] = chs(g.pstart)
	e[4] = 0x0C
	e[5], e[6], e[7] = chs(g.pstart + g.partSectors - 1)
	binary.LittleEndian.PutUint32(e[8:], g.pstart)
	binary.LittleEndian.PutUint32(e[12:], g.partSectors)
	s[510], s[511] = 0x55, 0xAA
	return s
}

func fsinfoSector(free, next uint32) []byte {
	fsi := make([]byte, 512)
	binary.LittleEndian.PutUint32(fsi[0:], 0x41615252)
	binary.LittleEndian.PutUint32(fsi[484:], 0x61417272)
	binary.LittleEndian.PutUint32(fsi[488:], free)
	binary.LittleEndian.PutUint32(fsi[492:], next)
	binary.LittleEndian.PutUint32(fsi[508:], 0xAA550000)
	return fsi
}

// writeEmptyFAT32 writes the VBR, FSInfo, their backups, both FATs and the root
// cluster holding the label entry. Field values follow mkfs.fat 4.2.
func writeEmptyFAT32(t Target, g geom, label string, volID uint32) error {
	pb := g.partByte()
	toZero := int64(g.reserved+2*g.fatlen+g.spc) * 512
	if err := zeroRange(t, pb, toZero); err != nil {
		return err
	}
	vbr := make([]byte, 512)
	copy(vbr, []byte{0xEB, 0x58, 0x90})
	copy(vbr[3:], "MSWIN4.1")
	le16 := func(off int, v uint16) { binary.LittleEndian.PutUint16(vbr[off:], v) }
	le32 := func(off int, v uint32) { binary.LittleEndian.PutUint32(vbr[off:], v) }
	le16(11, 512)
	vbr[13] = byte(g.spc)
	le16(14, uint16(g.reserved))
	vbr[16] = 2
	vbr[21] = 0xF8
	le16(24, 63)
	le16(26, uint16(g.heads))
	le32(28, g.pstart)
	le32(32, g.psize)
	le32(36, g.fatlen)
	le32(44, 2)
	le16(48, 1)
	le16(50, 6)
	vbr[64] = 0x80
	vbr[66] = 0x29
	le32(67, volID)
	copy(vbr[71:], fmt.Sprintf("%-11s", label))
	copy(vbr[82:], "FAT32   ")
	vbr[510], vbr[511] = 0x55, 0xAA
	fsi := fsinfoSector(g.clusters-1, 2)
	w := func(off int64, b []byte) error {
		_, err := t.WriteAt(b, pb+off)
		return err
	}
	for _, x := range []struct {
		off int64
		b   []byte
	}{{0, vbr}, {512, fsi}, {6 * 512, vbr}, {7 * 512, fsi}} {
		if err := w(x.off, x.b); err != nil {
			return err
		}
	}
	fat0 := make([]byte, 12)
	binary.LittleEndian.PutUint32(fat0[0:], 0x0FFFFFF8)
	binary.LittleEndian.PutUint32(fat0[4:], 0x0FFFFFFF)
	binary.LittleEndian.PutUint32(fat0[8:], 0x0FFFFFF8)
	if err := w(int64(g.reserved)*512, fat0); err != nil {
		return err
	}
	if err := w(int64(g.reserved+g.fatlen)*512, fat0); err != nil {
		return err
	}
	lbl := make([]byte, 32)
	copy(lbl, fmt.Sprintf("%-11s", label))
	lbl[11] = 0x08
	return w(int64(g.reserved+2*g.fatlen)*512, lbl)
}

// fsinfoRecount writes exact free-cluster figures into both FSInfo copies.
func fsinfoRecount(t Target, g geom) (uint32, uint32, error) {
	pb := g.partByte()
	fat := make([]byte, int(g.fatlen)*512)
	if _, err := t.ReadAt(fat, pb+int64(g.reserved)*512); err != nil {
		return 0, 0, err
	}
	var free uint32
	next := uint32(0xFFFFFFFF)
	for c := uint32(2); c < g.clusters+2; c++ {
		if binary.LittleEndian.Uint32(fat[c*4:])&0x0FFFFFFF == 0 {
			free++
			if next == 0xFFFFFFFF {
				next = c
			}
		}
	}
	fsi := fsinfoSector(free, next)
	if _, err := t.WriteAt(fsi, pb+512); err != nil {
		return 0, 0, err
	}
	if _, err := t.WriteAt(fsi, pb+7*512); err != nil {
		return 0, 0, err
	}
	return free, next, nil
}
