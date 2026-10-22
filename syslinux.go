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
//
// The boot-sector and ldlinux.sys patching in this file is a Go port of the
// syslinux installer library (libinstaller: syslxmod.c, fs.c, bootsect
// handling), Copyright 1998-2014 H. Peter Anvin and the Syslinux project,
// GPL-2.0-or-later; that is why this program is GPL.

package main

import (
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
)

// The BIOS payload: ldlinux.sys and ldlinux.bss of the exact syslinux build the
// ISO's modules come from, carved from Debian's syslinux -3.2 amd64 binary
// (offsets 25568 and 25056). See the design map, section 3, and the evidence
// directory. Both are GPLv2-or-later; this file is a port of syslinux's
// libinstaller (syslxmod.c, setadv.c, fs.c), also GPLv2-or-later.

//go:embed payload/ldlinux.sys
var payloadSys []byte

//go:embed payload/ldlinux.bss
var payloadBss []byte

//go:embed payload/ldlinux.c32.sha256
var expectedLdlinuxC32 string

const ldlinuxMagic = 0x3eb202fe
const ldlinuxPath = "boot/syslinux/ldlinux.sys"

func resetADV() []byte {
	a := make([]byte, 1024)
	for _, o := range []int{0, 512} {
		binary.LittleEndian.PutUint32(a[o:], 0x5a2d2fa5)
		binary.LittleEndian.PutUint32(a[o+4:], 0xa3041767)
		binary.LittleEndian.PutUint32(a[o+508:], 0xdd28bf64)
	}
	return a
}

func ldlinuxSysContent(core []byte) []byte {
	b := make([]byte, 0, len(core)+1024)
	b = append(b, core...)
	return append(b, resetADV()...)
}

func hasLdlinuxMagic(core []byte) bool {
	for off := 0; off+4 <= len(core) && off < 65536; off += 4 {
		if binary.LittleEndian.Uint32(core[off:]) == ldlinuxMagic {
			return true
		}
	}
	return false
}

// generateExtents packs data sectors into (lba u64, len u16) extents exactly as
// libinstaller's generate_extents does, including its 64 KiB-boundary test.
func generateExtents(ex []byte, exoff, nptrs int, sectp []uint64) (int, error) {
	for i := 0; i < nptrs*10; i++ {
		ex[exoff+i] = 0
	}
	addr := uint64(0x8000)
	base := addr
	var length uint16
	var lba uint64
	k := 0
	put := func() error {
		if k >= nptrs {
			return errors.New("insufficient extent space")
		}
		binary.LittleEndian.PutUint64(ex[exoff+10*k:], lba)
		binary.LittleEndian.PutUint16(ex[exoff+10*k+8:], length)
		k++
		return nil
	}
	for _, sect := range sectp {
		if length > 0 {
			xbytes := (uint64(length) + 1) * 512
			if sect == lba+uint64(length) && xbytes < 65536 && ((addr^(base+xbytes-1))&0xffff0000) == 0 {
				length++
				addr += 512
				continue
			}
			if err := put(); err != nil {
				return k, err
			}
		}
		base = addr
		lba = sect
		length = 1
		addr += 512
	}
	if length > 0 {
		if err := put(); err != nil {
			return k, err
		}
	}
	return k, nil
}

// syslinuxPatch patches the core image (img) and the boot-sector image (bs) for
// the given partition-relative sector list of ldlinux.sys. Port of syslinux_patch.
func syslinuxPatch(img, bs []byte, sectp []uint64, subdir string) (int, error) {
	L := len(img)
	nsect := (L+511)/512 + 2
	if len(sectp) < nsect {
		return 0, fmt.Errorf("ldlinux.sys sector list too short: %d < %d", len(sectp), nsect)
	}
	off := 0
	for binary.LittleEndian.Uint32(img[off:]) != ldlinuxMagic {
		off += 4
		if off+4 > L {
			return 0, errors.New("LDLINUX_MAGIC not found in payload")
		}
	}
	epao := int(binary.LittleEndian.Uint16(img[off+22:]))
	u16 := func(o int) int { return int(binary.LittleEndian.Uint16(img[o:])) }
	advptroffset, diroffset, dirlen := u16(epao), u16(epao+2), u16(epao+4)
	secptroffset, secptrcnt := u16(epao+10), u16(epao+12)
	sect1ptr0, sect1ptr1 := u16(epao+14), u16(epao+16)
	binary.LittleEndian.PutUint32(bs[sect1ptr0:], uint32(sectp[0]))
	binary.LittleEndian.PutUint32(bs[sect1ptr1:], uint32(sectp[0]>>32))
	sectp = sectp[1:]
	dw := L >> 2
	binary.LittleEndian.PutUint16(img[off+8:], uint16(nsect-2))
	binary.LittleEndian.PutUint16(img[off+10:], 2)
	binary.LittleEndian.PutUint32(img[off+12:], uint32(dw))
	if nsect > secptrcnt {
		return 0, errors.New("insufficient extent space")
	}
	nex, err := generateExtents(img, secptroffset, secptrcnt, sectp[:nsect-1-2])
	if err != nil {
		return 0, err
	}
	binary.LittleEndian.PutUint64(img[advptroffset:], sectp[nsect-1-2])
	binary.LittleEndian.PutUint64(img[advptroffset+8:], sectp[nsect-1-1])
	sb := append([]byte(subdir), 0)
	if dirlen < len(sb) {
		return 0, errors.New("subdirectory path too long")
	}
	copy(img[diroffset:], sb)
	binary.LittleEndian.PutUint32(img[off+16:], 0)
	csum := uint32(ldlinuxMagic)
	for i := 0; i < dw; i++ {
		csum -= binary.LittleEndian.Uint32(img[4*i:])
	}
	binary.LittleEndian.PutUint32(img[off+16:], csum)
	return nex, nil
}

// makeBootsect merges the syslinux boot-sector code into the real VBR, keeping the BPB.
func makeBootsect(vbr, bs []byte) []byte {
	v := make([]byte, 512)
	copy(v, vbr)
	copy(v[0:11], bs[0:11])
	copy(v[90:512], bs[90:512])
	return v
}

// installSyslinux patches the already-written ldlinux.sys in place, marks it
// read-only/hidden/system, and writes the boot sector to partition sectors 0 and 6.
func installSyslinux(f *fatFS, core, bss []byte) error {
	d, err := f.lookup(ldlinuxPath)
	if err != nil {
		return err
	}
	if int(d.Size) != len(core)+1024 {
		return fmt.Errorf("ldlinux.sys on target is %d bytes, expected %d", d.Size, len(core)+1024)
	}
	secs, err := f.fileSectors(d)
	if err != nil {
		return err
	}
	sectp := make([]uint64, len(secs))
	for i, s := range secs {
		sectp[i] = uint64(s)
	}
	img := append([]byte(nil), core...)
	bs := append([]byte(nil), bss...)
	if _, err := syslinuxPatch(img, bs, sectp, "/boot/syslinux"); err != nil {
		return err
	}
	for i := 0; i < len(img)/512; i++ {
		if err := f.writeSectors(secs[i], img[i*512:(i+1)*512]); err != nil {
			return err
		}
	}
	if err := f.setAttr(d, 0x07); err != nil {
		return err
	}
	vbr, err := f.readSectors(0, 1)
	if err != nil {
		return err
	}
	merged := makeBootsect(vbr, bs)
	if err := f.writeSectors(0, merged); err != nil {
		return err
	}
	return f.writeSectors(6, merged)
}
