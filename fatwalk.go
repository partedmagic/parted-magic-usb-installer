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
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
)

// fatFS is the core's own read-side view of the FAT32 partition. It is
// independent of go-diskfs so that a writer bug is not mirrored in verification.
type fatFS struct {
	t        Target
	pb       int64
	spc      uint32
	reserved uint32
	nfats    uint32
	fatsz    uint32
	rootc    uint32
	totsec   uint32
	data0    uint32
	clusters uint32
	fat      []byte
}

type dirent struct {
	Name, Short string
	Attr        byte
	First, Size uint32
	entSec      uint32 // partition-relative sector holding the short entry
	entOff      int
	lfnSecs     []uint32 // locations of the long-name entries preceding it
	lfnOffs     []int
}

func openFAT(t Target, pb int64) (*fatFS, error) {
	bs := make([]byte, 512)
	if _, err := t.ReadAt(bs, pb); err != nil {
		return nil, err
	}
	if binary.LittleEndian.Uint16(bs[11:]) != 512 || bs[510] != 0x55 || bs[511] != 0xAA {
		return nil, errors.New("no FAT32 boot sector at the partition start")
	}
	f := &fatFS{t: t, pb: pb, spc: uint32(bs[13]), reserved: uint32(binary.LittleEndian.Uint16(bs[14:])),
		nfats: uint32(bs[16]), fatsz: binary.LittleEndian.Uint32(bs[36:]), rootc: binary.LittleEndian.Uint32(bs[44:]),
		totsec: binary.LittleEndian.Uint32(bs[32:])}
	if f.spc == 0 || f.fatsz == 0 || f.nfats == 0 {
		return nil, errors.New("invalid FAT32 boot sector")
	}
	f.data0 = f.reserved + f.nfats*f.fatsz
	f.clusters = (f.totsec - f.data0) / f.spc
	f.fat = make([]byte, int(f.fatsz)*512)
	if _, err := t.ReadAt(f.fat, pb+int64(f.reserved)*512); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *fatFS) readSectors(sec uint32, n int) ([]byte, error) {
	b := make([]byte, n*512)
	_, err := f.t.ReadAt(b, f.pb+int64(sec)*512)
	return b, err
}
func (f *fatFS) writeSectors(sec uint32, b []byte) error {
	_, err := f.t.WriteAt(b, f.pb+int64(sec)*512)
	return err
}
func (f *fatFS) sectorOfCluster(c uint32) uint32 { return f.data0 + (c-2)*f.spc }
func (f *fatFS) next(c uint32) uint32 {
	return binary.LittleEndian.Uint32(f.fat[c*4:]) & 0x0FFFFFFF
}

func (f *fatFS) chain(c uint32) ([]uint32, error) {
	var out []uint32
	for c >= 2 && c < 0x0FFFFFF8 {
		if c-2 >= f.clusters {
			return nil, fmt.Errorf("cluster %d out of range", c)
		}
		out = append(out, c)
		if uint32(len(out)) > f.clusters {
			return nil, errors.New("cluster chain loops")
		}
		c = f.next(c)
	}
	return out, nil
}

func shortName(e []byte) string {
	b := make([]byte, 11)
	copy(b, e[:11])
	if b[0] == 0x05 {
		b[0] = 0xE5
	}
	base := strings.TrimRight(string(b[:8]), " ")
	ext := strings.TrimRight(string(b[8:11]), " ")
	if e[12]&0x08 != 0 {
		base = strings.ToLower(base)
	}
	if e[12]&0x10 != 0 {
		ext = strings.ToLower(ext)
	}
	if ext != "" {
		return base + "." + ext
	}
	return base
}

func lfnChecksum(short []byte) byte {
	var sum byte
	for i := 0; i < 11; i++ {
		sum = ((sum & 1) << 7) + (sum >> 1) + short[i]
	}
	return sum
}

func (f *fatFS) readDir(c0 uint32) ([]dirent, error) {
	ch, err := f.chain(c0)
	if err != nil {
		return nil, err
	}
	var out []dirent
	parts := map[int][]uint16{}
	var lfnSum byte
	var lfnSecs []uint32
	var lfnOffs []int
	for _, c := range ch {
		sec := f.sectorOfCluster(c)
		d, err := f.readSectors(sec, int(f.spc))
		if err != nil {
			return nil, err
		}
		for i := 0; i < len(d); i += 32 {
			e := d[i : i+32]
			if e[0] == 0 {
				return out, nil
			}
			if e[0] == 0xE5 {
				parts = map[int][]uint16{}
				lfnSecs, lfnOffs = nil, nil
				continue
			}
			if e[11] == 0x0F {
				seq := int(e[0] & 0x1F)
				if e[0]&0x40 != 0 {
					parts = map[int][]uint16{}
					lfnSecs, lfnOffs = nil, nil
					lfnSum = e[13]
				}
				lfnSecs = append(lfnSecs, sec+uint32(i/512))
				lfnOffs = append(lfnOffs, i%512)
				var cs []uint16
				for _, off := range []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30} {
					cs = append(cs, binary.LittleEndian.Uint16(e[off:]))
				}
				parts[seq] = cs
				continue
			}
			if e[11]&0x08 != 0 { // volume label
				parts = map[int][]uint16{}
				lfnSecs, lfnOffs = nil, nil
				continue
			}
			de := dirent{Short: shortName(e), Attr: e[11],
				First: uint32(binary.LittleEndian.Uint16(e[20:]))<<16 | uint32(binary.LittleEndian.Uint16(e[26:])),
				Size:  binary.LittleEndian.Uint32(e[28:]), entSec: sec + uint32(i/512), entOff: i % 512}
			de.Name = de.Short
			if len(parts) > 0 && lfnChecksum(e[:11]) == lfnSum {
				de.lfnSecs, de.lfnOffs = lfnSecs, lfnOffs
				var all []uint16
				for k := 1; k <= len(parts); k++ {
					p, ok := parts[k]
					if !ok {
						all = nil
						break
					}
					all = append(all, p...)
				}
				if all != nil {
					for j, u := range all {
						if u == 0 {
							all = all[:j]
							break
						}
					}
					n := len(all)
					for n > 0 && all[n-1] == 0xFFFF {
						n--
					}
					de.Name = string(utf16.Decode(all[:n]))
				}
			}
			parts = map[int][]uint16{}
			lfnSecs, lfnOffs = nil, nil
			out = append(out, de)
		}
	}
	return out, nil
}

func (f *fatFS) lookup(p string) (*dirent, error) {
	c := f.rootc
	comps := strings.Split(strings.Trim(p, "/"), "/")
	var cur *dirent
	for i, comp := range comps {
		ents, err := f.readDir(c)
		if err != nil {
			return nil, err
		}
		cur = nil
		for k := range ents {
			if strings.EqualFold(ents[k].Name, comp) || strings.EqualFold(ents[k].Short, comp) {
				cur = &ents[k]
				break
			}
		}
		if cur == nil {
			return nil, fmt.Errorf("%s: not found on the target", p)
		}
		if i < len(comps)-1 {
			if cur.Attr&0x10 == 0 {
				return nil, fmt.Errorf("%s: %s is not a directory", p, comp)
			}
			c = cur.First
		}
	}
	return cur, nil
}

func (f *fatFS) fileClusters(d *dirent) ([]uint32, error) {
	if d.Size == 0 && d.First == 0 {
		return nil, nil
	}
	return f.chain(d.First)
}

// fileSectors returns the partition-relative sectors holding the file, in order,
// truncated to the file size.
func (f *fatFS) fileSectors(d *dirent) ([]uint32, error) {
	ch, err := f.fileClusters(d)
	if err != nil {
		return nil, err
	}
	need := int((uint64(d.Size) + 511) / 512)
	secs := make([]uint32, 0, need)
	for _, c := range ch {
		s := f.sectorOfCluster(c)
		for k := uint32(0); k < f.spc && len(secs) < need; k++ {
			secs = append(secs, s+k)
		}
	}
	if len(secs) < need {
		return nil, fmt.Errorf("%s: chain shorter than the file size", d.Name)
	}
	return secs, nil
}

func (f *fatFS) readFile(d *dirent, w io.Writer) (int64, error) {
	ch, err := f.fileClusters(d)
	if err != nil {
		return 0, err
	}
	left := int64(d.Size)
	var n int64
	cb := int64(f.spc) * 512
	for i := 0; i < len(ch) && left > 0; {
		// coalesce contiguous clusters
		j := i + 1
		for j < len(ch) && ch[j] == ch[j-1]+1 && int64(j-i)*cb < 4<<20 {
			j++
		}
		b, err := f.readSectors(f.sectorOfCluster(ch[i]), int(f.spc)*(j-i))
		if err != nil {
			return n, err
		}
		if int64(len(b)) > left {
			b = b[:left]
		}
		if _, err := w.Write(b); err != nil {
			return n, err
		}
		n += int64(len(b))
		left -= int64(len(b))
		i = j
	}
	if left != 0 {
		return n, fmt.Errorf("%s: chain ends before the file size", d.Name)
	}
	return n, nil
}

func (f *fatFS) setAttr(d *dirent, attr byte) error {
	b, err := f.readSectors(d.entSec, 1)
	if err != nil {
		return err
	}
	b[d.entOff+11] = attr
	return f.writeSectors(d.entSec, b)
}
