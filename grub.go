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
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// GRUB for BIOS boot (ISOs from 26.10_4 on).  The ISO ships GRUB's own
// i386-pc boot.img (the MBR code) and core.img (made with grub-mkimage
// -O i386-pc -p '(,msdos1)/boot/grub', running boot/grub/bios.cfg).  Both come
// from the ISO, so they always match its modules, and no GRUB code is part of
// this program: it places the two images the way grub-bios-setup does for a
// hard disk, in the gap pmusb leaves before the partition at LBA 2048.
//
//	sector 0     boot.img's first 440 bytes, then the disk signature and the
//	             partition table this program writes anyway
//	sector 1...  core.img, as built: its blocklist already says "the rest
//	             follows at sector 2", and boot.img's kernel sector is 1
//
// The one change to boot.img is the one grub-bios-setup makes for a hard
// disk: the jump at boot_drive_check becomes two NOPs, so a BIOS that passes
// a bad drive number in DL still boots the stick as 0x80.
const (
	grubDir           = "boot/grub/i386-pc"
	grubBootImg       = grubDir + "/boot.img"
	grubCoreImg       = grubDir + "/core.img"
	grubKernelSector  = 0x5c  // GRUB_BOOT_MACHINE_KERNEL_SECTOR, 8 bytes
	grubDriveCheck    = 0x66  // GRUB_BOOT_MACHINE_DRIVE_CHECK, 2 bytes
	grubBlocklist     = 0x1f4 // diskboot.img: start (8), len (2), segment (2)
	grubCoreLBA       = 1
	grubCoreMaxSector = 2047 // the partition starts at 2048
)

// Boot loaders a stick can carry.
const (
	loaderNone     = ""
	loaderSyslinux = "syslinux"
	loaderGRUB     = "grub"
)

// checkGRUB reads and checks the ISO's boot.img and core.img.  The checks are
// the facts the placement above depends on; anything else means the ISO's
// GRUB files are not the ones this program knows how to place.
func checkGRUB(boot, core []byte) error {
	if len(boot) != 512 || boot[510] != 0x55 || boot[511] != 0xAA {
		return fmt.Errorf("%s is not a 512-byte boot sector", grubBootImg)
	}
	if binary.LittleEndian.Uint64(boot[grubKernelSector:]) != grubCoreLBA {
		return fmt.Errorf("%s does not load its core from sector %d", grubBootImg, grubCoreLBA)
	}
	if !(boot[grubDriveCheck] == 0xEB || (boot[grubDriveCheck] == 0x90 && boot[grubDriveCheck+1] == 0x90)) {
		return fmt.Errorf("%s has no drive check where GRUB puts it", grubBootImg)
	}
	if len(core) < 1024 {
		return fmt.Errorf("%s is too small to be a GRUB core image", grubCoreImg)
	}
	secs := (len(core) + 511) / 512
	if secs > grubCoreMaxSector {
		return fmt.Errorf("%s is %d sectors; at most %d fit before the partition", grubCoreImg, secs, grubCoreMaxSector)
	}
	if start := binary.LittleEndian.Uint64(core[grubBlocklist:]); start != grubCoreLBA+1 {
		return fmt.Errorf("%s: its blocklist starts at sector %d, not %d", grubCoreImg, start, grubCoreLBA+1)
	}
	if n := int(binary.LittleEndian.Uint16(core[grubBlocklist+8:])); n != secs-1 {
		return fmt.Errorf("%s: its blocklist covers %d sectors, the image needs %d", grubCoreImg, n, secs-1)
	}
	return nil
}

// grubMBRCode is the 440 bytes of boot code for sector 0.
func grubMBRCode(boot []byte) []byte {
	code := append([]byte(nil), boot[:440]...)
	code[grubDriveCheck], code[grubDriveCheck+1] = 0x90, 0x90
	return code
}

// grubCoreSectors pads core.img to whole sectors.
func grubCoreSectors(core []byte) []byte {
	b := make([]byte, (len(core)+511)/512*512)
	copy(b, core)
	return b
}

func grubCoreHash(core []byte) string {
	h := sha256.Sum256(grubCoreSectors(core))
	return hex.EncodeToString(h[:])
}

// installGRUB writes core.img into the gap and reads it back.  Sector 0 is
// written last, by the caller, with the partition table.
func installGRUB(t Target, core []byte) error {
	b := grubCoreSectors(core)
	if _, err := t.WriteAt(b, grubCoreLBA*512); err != nil {
		return err
	}
	if err := t.Sync(); err != nil {
		return err
	}
	back := make([]byte, len(b))
	if _, err := t.ReadAt(back, grubCoreLBA*512); err != nil {
		return err
	}
	if !bytes.Equal(back, b) {
		return fmt.Errorf("GRUB core image read-back mismatch")
	}
	return nil
}

// verifyGRUB checks the core image in the gap against the hash in the
// manifest, and that sector 0 carries GRUB's code with the drive check off.
func verifyGRUB(t Target, coreHash string, coreSectors int) error {
	if coreSectors < 2 || coreSectors > grubCoreMaxSector {
		return fmt.Errorf("manifest gives a GRUB core of %d sectors", coreSectors)
	}
	b := make([]byte, coreSectors*512)
	if _, err := t.ReadAt(b, grubCoreLBA*512); err != nil {
		return err
	}
	h := sha256.Sum256(b)
	if hex.EncodeToString(h[:]) != coreHash {
		return fmt.Errorf("the GRUB core image in sectors %d-%d differs from the source", grubCoreLBA, coreSectors)
	}
	return nil
}

// verifyGRUBMBR checks the boot code in sector 0 once it is written.
func verifyGRUBMBR(sec0 []byte) error {
	if binary.LittleEndian.Uint64(sec0[grubKernelSector:]) != grubCoreLBA || sec0[grubDriveCheck] != 0x90 || sec0[grubDriveCheck+1] != 0x90 {
		return fmt.Errorf("sector 0 does not carry GRUB's boot code")
	}
	return nil
}

// bootInfo is what the read-back verification needs to know about the BIOS
// boot loader on a stick.  Loader is loaderNone for a UEFI-only stick.
type bootInfo struct {
	Loader      string
	CoreHash    string
	CoreSectors int
}

func (s *Source) bootInfo(bios bool) bootInfo {
	if !bios {
		return bootInfo{}
	}
	if s.Loader == loaderGRUB {
		return bootInfo{Loader: loaderGRUB, CoreHash: grubCoreHash(s.GrubCore), CoreSectors: len(grubCoreSectors(s.GrubCore)) / 512}
	}
	return bootInfo{Loader: loaderSyslinux}
}

// manifestHeader is the manifest up to its file list.  "loader" and
// "grubcore" are new with GRUB; a manifest without them and "bios true" was
// written for syslinux.
func manifestHeader(s *Source, bi bootInfo) string {
	var mb bytes.Buffer
	fmt.Fprintf(&mb, "pmusb-manifest 1\ninstaller %s\npayload %s\nlabel %s\nversion %s\nbuild %s\nsqfs %s\nbios %v\n",
		version, s.payloadDesc(), s.Label, s.Version, s.Build, s.SQFS, bi.Loader != loaderNone)
	if bi.Loader != loaderNone {
		fmt.Fprintf(&mb, "loader %s\n", bi.Loader)
	}
	if bi.Loader == loaderGRUB {
		fmt.Fprintf(&mb, "grubcore %s %d\n", bi.CoreHash, bi.CoreSectors)
	}
	mb.WriteString("files:\n")
	return mb.String()
}

// parseBootLine reads the manifest lines manifestHeader writes about the
// boot loader.  It returns false for any other line.
func parseBootLine(line string, bi *bootInfo) bool {
	switch {
	case strings.HasPrefix(line, "bios "):
		if strings.TrimSpace(line[5:]) == "true" && bi.Loader == loaderNone {
			bi.Loader = loaderSyslinux
		}
	case strings.HasPrefix(line, "loader "):
		bi.Loader = strings.TrimSpace(line[7:])
	case strings.HasPrefix(line, "grubcore "):
		f := strings.Fields(line[9:])
		if len(f) == 2 {
			bi.CoreHash = f[0]
			bi.CoreSectors, _ = strconv.Atoi(f[1])
		}
	default:
		return false
	}
	return true
}

// writeGRUBMBRCode puts GRUB's boot code into an existing sector 0, keeping
// its disk signature and partition table (an upgrade from a syslinux stick).
func writeGRUBMBRCode(t Target, boot []byte) error {
	sec0 := make([]byte, 512)
	if _, err := t.ReadAt(sec0, 0); err != nil {
		return err
	}
	copy(sec0, grubMBRCode(boot))
	if _, err := t.WriteAt(sec0, 0); err != nil {
		return err
	}
	if err := t.Sync(); err != nil {
		return err
	}
	back := make([]byte, 512)
	if _, err := t.ReadAt(back, 0); err != nil {
		return err
	}
	if !bytes.Equal(back, sec0) {
		return fmt.Errorf("sector 0 read-back mismatch")
	}
	return verifyGRUBMBR(back)
}
