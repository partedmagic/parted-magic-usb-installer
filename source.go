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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/diskfs/go-diskfs/backend"
	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

type entry struct {
	p    string
	size int64
	dir  bool
}

type srcFS interface {
	readDir(dir string) ([]fs.DirEntry, error)
	open(p string) (io.ReadCloser, error)
	label() string
	close() error
}

type isoSrc struct {
	fsys *iso9660.FileSystem
}

func (s *isoSrc) readDir(dir string) ([]fs.DirEntry, error) { return s.fsys.ReadDir(dir) }
func (s *isoSrc) open(p string) (io.ReadCloser, error)      { return s.fsys.OpenFile(p, os.O_RDONLY) }
func (s *isoSrc) label() string                             { return strings.TrimRight(s.fsys.Label(), " ") }
func (s *isoSrc) close() error                              { return s.fsys.Close() }

type dirSrc struct{ base string }

func (s *dirSrc) readDir(dir string) ([]fs.DirEntry, error) {
	return os.ReadDir(filepath.Join(s.base, filepath.FromSlash(dir)))
}
func (s *dirSrc) open(p string) (io.ReadCloser, error) {
	return os.Open(filepath.Join(s.base, filepath.FromSlash(p)))
}
func (s *dirSrc) label() string { return "" }
func (s *dirSrc) close() error  { return nil }

type Source struct {
	path        string
	fs          srcFS
	Label       string
	entries     []entry
	byPath      map[string]*entry
	SQFS        string
	Version     string
	Build       string
	Files, Dirs int
	Bytes       int64
	ModuleMatch bool
	moduleHash  string
	// BIOS payload: from the source's own boot/syslinux/ldlinux.sys + ldlinux.bss
	// when the ISO ships them, else the copy embedded in this program.
	PayloadSys  []byte
	PayloadBss  []byte
	PayloadFrom string
	// Loader is the BIOS boot loader the ISO is made for: loaderGRUB when it
	// ships boot/grub/i386-pc with boot.img and core.img (26.10_4 on), else
	// loaderSyslinux.  GrubBoot and GrubCore are the ISO's own images.
	Loader   string
	GrubBoot []byte
	GrubCore []byte
	// BootLabel is set when the ISO's boot menu finds its medium by the
	// volume label: the stick must carry that label, whatever --label says.
	BootLabel string
}

// Two release-naming schemes: the date form used until 2026-09
// (PM_2026_06_04, optionally _n for a rebuild) and the year.month form
// from 26.09 on (PM_26.09).  The squashfs carries the same version string.
var labelRE = regexp.MustCompile(`^PM_(\d{4}_\d{2}_\d{2}|\d{2}\.\d{2})(?:_(\d+))?$`)

// The squashfs carries the SAME string as the label, point release and all:
// build names it PMAGIC_$version.SQFS from the same $version that becomes the
// volume label, so a rebuild of 26.09 is labelled PM_26.09_1 and holds
// PMAGIC_26.09_1.SQFS.  Without the optional _n here the label passed and the
// squashfs was not found at all ("found 0"), which refused every point release
// in both naming schemes -- caught 2026-09-18 on pmagic_26.09_1.iso.
var sqfsRE = regexp.MustCompile(`^pmagic/pmodules/PMAGIC_(\d{4}_\d{2}_\d{2}|\d{2}\.\d{2})(?:_(\d+))?\.SQFS$`)

// sourceFile lets a platform hand over the ISO as a file that is already
// open: an Android app gets a descriptor from the system file picker, not a
// path it may open. It returns nil, nil for a path it does not handle.
var sourceFile func(p string) (*os.File, error)

func openSource(p string) (*Source, error) {
	var opened *os.File
	if sourceFile != nil {
		f, err := sourceFile(p)
		if err != nil {
			return nil, err
		}
		opened = f
	}
	var st os.FileInfo
	var err error
	if opened != nil {
		st, err = opened.Stat()
	} else {
		st, err = os.Stat(p)
	}
	if err != nil {
		return nil, err
	}
	s := &Source{path: p, byPath: map[string]*entry{}}
	if st.IsDir() {
		s.fs = &dirSrc{base: p}
	} else {
		var b backend.Storage
		if opened != nil {
			b = file.New(opened, true)
		} else if b, err = file.OpenFromPath(p, true); err != nil {
			return nil, err
		}
		fsys, err := iso9660.Read(b, st.Size(), 0, 2048)
		if err != nil {
			return nil, fmt.Errorf("not an ISO 9660 image: %w", err)
		}
		s.fs = &isoSrc{fsys: fsys}
	}
	s.Label = s.fs.label()
	if err := s.walk(".", &s.entries); err != nil {
		s.Close()
		return nil, err
	}
	for i := range s.entries {
		e := &s.entries[i]
		s.byPath[e.p] = e
		if e.dir {
			s.Dirs++
		} else {
			s.Files++
			s.Bytes += e.size
		}
	}
	return s, nil
}

func (s *Source) Close() error { return s.fs.close() }

func (s *Source) walk(dir string, out *[]entry) error {
	ents, err := s.fs.readDir(dir)
	if err != nil {
		return fmt.Errorf("readdir %s: %w", dir, err)
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	for _, e := range ents {
		name := e.Name()
		if name == "." || name == ".." {
			continue
		}
		full := name
		if dir != "." {
			full = path.Join(dir, name)
		}
		if e.IsDir() {
			*out = append(*out, entry{p: full, dir: true})
			if err := s.walk(full, out); err != nil {
				return err
			}
			continue
		}
		var sz int64
		if fi, err := e.Info(); err == nil {
			sz = fi.Size()
		}
		*out = append(*out, entry{p: full, size: sz})
	}
	return nil
}

func (s *Source) readAll(p string) ([]byte, error) {
	r, err := s.fs.open(p)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func (s *Source) has(p string) bool { _, ok := s.byPath[p]; return ok }

// labelVersion puts a version and its point release back together the way the
// label spells them, for messages: ("26.09", "1") -> "26.09_1".
func labelVersion(version, build string) string {
	if build == "" {
		return version
	}
	return version + "_" + build
}

// efiBootRE is the removable-media boot file UEFI firmware starts from a
// stick: \EFI\BOOT\BOOTX64.EFI on a PC, BOOTAA64.EFI on arm64, and so on.
var efiBootRE = regexp.MustCompile(`(?i)^efi/boot/boot[a-z0-9]+\.efi$`)

// diskInfoRE reads a version out of /.disk/info ("Parted Magic 26.10 arm64 ...").
var diskInfoRE = regexp.MustCompile(`Parted Magic ([0-9][0-9A-Za-z._-]*)`)

// validate decides whether the source can make a stick that starts a
// computer, and nothing else.  It refuses a file FAT32 cannot hold (4 GiB or
// more), a Rock Ridge relocation it cannot copy, and an ISO with nothing to
// boot: no EFI\BOOT\BOOT*.EFI and no BIOS boot loader.  The label, the
// version, the squashfs name and where the kernel sits are read as found,
// for messages and the manifest only.  Until 2026-10-26 every one of them
// was a rule, so an ISO laid out any other way than an x86 release (a new
// name, a moved file, the arm64 ISO) needed a new installer.  Patrick: "It's
// so picky I'm surprised we don't have to make a new release for every new
// iso name."
func (s *Source) validate() error {
	var probs []string
	// The version: from the label (PM_26.10_4), else from a PMAGIC_*.SQFS
	// name, else from /.disk/info.
	if m := labelRE.FindStringSubmatch(s.Label); m != nil {
		s.Version, s.Build = m[1], m[2]
	}
	var sqfs []string
	var biggest *entry // any other squashfs: casper/filesystem.squashfs on arm64
	efi := false
	for i := range s.entries {
		e := &s.entries[i]
		if m := sqfsRE.FindStringSubmatch(e.p); m != nil {
			sqfs = append(sqfs, e.p)
			if s.Version == "" {
				s.Version, s.Build = m[1], m[2]
			}
		} else if lp := strings.ToLower(e.p); !e.dir && (strings.HasSuffix(lp, ".sqfs") || strings.HasSuffix(lp, ".squashfs")) {
			if biggest == nil || e.size > biggest.size {
				biggest = e
			}
		}
		if !e.dir && efiBootRE.MatchString(e.p) {
			efi = true
		}
		if !e.dir && e.size >= 1<<32 {
			probs = append(probs, fmt.Sprintf("%s is %d bytes; FAT32 files must be under 4 GiB", e.p, e.size))
		}
		if e.dir && path.Base(e.p) == ".rr_moved" {
			probs = append(probs, "ISO contains a .rr_moved directory (deep-directory relocation); refusing rather than guessing")
		}
	}
	// The squashfs is copied last, in one long run.  Of several PMAGIC_*.SQFS
	// the one of the label's version, else the first.
	switch {
	case len(sqfs) > 0:
		s.SQFS = sqfs[0]
		for _, p := range sqfs {
			if m := sqfsRE.FindStringSubmatch(p); m[1] == s.Version && m[2] == s.Build {
				s.SQFS = p
				break
			}
		}
	case biggest != nil:
		s.SQFS = biggest.p
	}
	if s.Version == "" {
		if b, err := s.readAll(".disk/info"); err == nil {
			if m := diskInfoRE.FindStringSubmatch(string(b)); m != nil {
				s.Version = m[1]
			}
		}
	}
	// The BIOS boot loader: GRUB when the ISO ships its BIOS files (26.10_4
	// on), else syslinux when it ships those (26.10_3 and older), else none
	// and the stick starts on UEFI only, as an arm64 one does.
	switch {
	case s.has(grubBootImg) && s.has(grubCoreImg) && s.has(grubDir+"/normal.mod") && s.has("boot/grub/bios.cfg"):
		boot, err1 := s.readAll(grubBootImg)
		core, err2 := s.readAll(grubCoreImg)
		if err1 != nil || err2 != nil {
			probs = append(probs, "GRUB's boot.img or core.img is unreadable")
		} else if err := checkGRUB(boot, core); err != nil {
			probs = append(probs, err.Error())
		} else {
			s.Loader, s.GrubBoot, s.GrubCore = loaderGRUB, boot, core
		}
	case s.has("boot/syslinux/syslinux.cfg") && s.has("boot/syslinux/ldlinux.c32") && s.has("boot/syslinux/mbr.bin"):
		if e := s.byPath["boot/syslinux/mbr.bin"]; e.size != 440 {
			probs = append(probs, "boot/syslinux/mbr.bin is not 440 bytes")
		} else {
			s.Loader = loaderSyslinux
		}
	}
	if sys, err := s.readAll("boot/syslinux/ldlinux.sys"); err == nil {
		bss, err2 := s.readAll("boot/syslinux/ldlinux.bss")
		if err2 == nil && len(bss) == 512 && len(sys) >= 4096 && len(sys)%512 == 0 && hasLdlinuxMagic(sys) {
			s.PayloadSys, s.PayloadBss, s.PayloadFrom = sys, bss, "source"
		} else if s.Loader == loaderSyslinux {
			probs = append(probs, "boot/syslinux/ldlinux.sys is present but it and ldlinux.bss are not a usable syslinux payload")
		}
	}
	if s.PayloadFrom == "" {
		s.PayloadSys, s.PayloadBss, s.PayloadFrom = payloadSys, payloadBss, "embedded"
	}
	if b, err := s.readAll("boot/syslinux/ldlinux.c32"); err == nil {
		h := sha256.Sum256(b)
		s.moduleHash = hex.EncodeToString(h[:])
		// A payload shipped beside the modules matches them by construction.
		s.ModuleMatch = s.PayloadFrom == "source" || s.moduleHash == strings.TrimSpace(expectedLdlinuxC32)
	}
	if s.Loader != loaderSyslinux {
		// GRUB's boot.img and core.img come from the ISO beside its modules,
		// and a UEFI-only stick has no BIOS modules to match.
		s.ModuleMatch = true
	}
	if !efi && s.Loader == loaderNone {
		probs = append(probs, "nothing on it starts a computer: no EFI/BOOT/BOOT*.EFI and no BIOS boot loader")
	}
	// An ISO whose boot menu finds its medium by the volume label (the arm64
	// one: live-media=/dev/disk/by-label/PM_ARM64) needs that label on the
	// stick too.
	if s.Label != "" {
		if b, err := s.readAll("boot/grub/grub.cfg"); err == nil {
			cfg := string(b)
			for _, ref := range []string{"by-label/" + s.Label, "--label " + s.Label, "label=" + s.Label} {
				if strings.Contains(cfg, ref) {
					if l, err := normLabel(s.Label); err == nil {
						s.BootLabel = l
					}
					break
				}
			}
		}
	}
	if len(probs) > 0 {
		return errors.New("not a usable Parted Magic source: " + strings.Join(probs, "; "))
	}
	return nil
}

type summary struct {
	Source, Label, Version, Build, SQFS string
	Files, Dirs                         int
	Bytes, RequiredBytes                int64
	BIOSModulesMatch                    bool
	Payload, PayloadFrom                string
	BIOSLoader                          string
	StickLabel                          string `json:",omitempty"`
}

func (s *Source) payloadDesc() string {
	if s.Loader == loaderGRUB {
		return "GRUB boot.img and core.img shipped in the source (boot/grub/i386-pc)"
	}
	if s.Loader == loaderNone {
		return "none (the source has no BIOS boot loader; UEFI only)"
	}
	if s.PayloadFrom == "source" {
		return "ldlinux.sys and ldlinux.bss shipped in the source (boot/syslinux)"
	}
	return payloadName
}

// name is what messages call the source: "Parted Magic 26.10", else its
// volume label, else its file name.
func (s *Source) name() string {
	switch {
	case s.Version != "":
		return "Parted Magic " + s.Version
	case s.Label != "":
		return s.Label
	}
	return filepath.Base(s.path)
}

func (s *Source) summary() summary {
	return summary{Source: s.path, Label: s.Label, Version: s.Version, Build: s.Build, SQFS: s.SQFS,
		Files: s.Files, Dirs: s.Dirs, Bytes: s.Bytes, RequiredBytes: requiredBytes(s.entries, 32*1024),
		BIOSModulesMatch: s.ModuleMatch, Payload: s.payloadDesc(), PayloadFrom: s.PayloadFrom, BIOSLoader: s.Loader,
		StickLabel: s.BootLabel}
}

// skipped: ISO-only artifacts that must not land on a stick (design section 2).
func skipped(p string) bool {
	switch strings.ToLower(p) {
	case "efi/boot/efiboot.img", "boot/syslinux/boot.cat", "efi/boot/fbx64.efi":
		return true
	}
	return false
}
