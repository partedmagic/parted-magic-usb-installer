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

// repairTree fixes what go-diskfs v1.9.4 gets wrong on the populated volume
// (design section 9, defects 3 to 5), through the core's own FAT writer:
//   - zero-length files must own no cluster,
//   - the ".." entry of a directory under the root must point at cluster 0,
//   - a short name may not start with a dot (".disk" got ".DIS").
//
// fsck.fat and Windows expect all three; the boot chains did not care.
func repairTree(f *fatFS) error {
	touched := map[uint32]bool{} // FAT sectors to write back to both copies
	freeChain := func(first uint32) error {
		ch, err := f.chain(first)
		if err != nil {
			return err
		}
		for _, c := range ch {
			binary.LittleEndian.PutUint32(f.fat[c*4:], 0)
			touched[c*4/512] = true
		}
		return nil
	}
	patchEntry := func(d *dirent, fn func(e []byte)) error {
		b, err := f.readSectors(d.entSec, 1)
		if err != nil {
			return err
		}
		fn(b[d.entOff : d.entOff+32])
		return f.writeSectors(d.entSec, b)
	}
	var walk func(c uint32, depth int) error
	walk = func(c uint32, depth int) error {
		ents, err := f.readDir(c)
		if err != nil {
			return err
		}
		shorts := map[string]bool{}
		for i := range ents {
			shorts[strings.ToUpper(ents[i].Short)] = true
		}
		for i := range ents {
			d := &ents[i]
			if d.Short == "." || d.Short == ".." {
				if d.Short == ".." && depth == 1 && d.First != 0 {
					if err := patchEntry(d, func(e []byte) {
						binary.LittleEndian.PutUint16(e[20:], 0)
						binary.LittleEndian.PutUint16(e[26:], 0)
					}); err != nil {
						return err
					}
				}
				continue
			}
			if d.Attr&0x10 == 0 && d.Size == 0 && d.First != 0 {
				if err := freeChain(d.First); err != nil {
					return err
				}
				if err := patchEntry(d, func(e []byte) {
					binary.LittleEndian.PutUint16(e[20:], 0)
					binary.LittleEndian.PutUint16(e[26:], 0)
				}); err != nil {
					return err
				}
			}
			if strings.HasPrefix(d.Short, ".") && d.Name != "." && d.Name != ".." {
				base := strings.ToUpper(strings.TrimLeft(d.Name, "."))
				var sb strings.Builder
				for _, ch := range base {
					if (ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') && sb.Len() < 6 {
						sb.WriteRune(ch)
					}
				}
				if sb.Len() == 0 {
					sb.WriteString("DOT")
				}
				var short string
				for n := 1; n < 1000; n++ {
					short = fmt.Sprintf("%s~%d", sb.String(), n)
					if !shorts[short] {
						break
					}
				}
				shorts[short] = true
				raw := []byte(fmt.Sprintf("%-8s   ", short))[:11]
				sum := lfnChecksum(raw)
				if err := patchEntry(d, func(e []byte) {
					copy(e[:11], raw)
					e[12] = 0
				}); err != nil {
					return err
				}
				for k := range d.lfnSecs {
					b, err := f.readSectors(d.lfnSecs[k], 1)
					if err != nil {
						return err
					}
					b[d.lfnOffs[k]+13] = sum
					if err := f.writeSectors(d.lfnSecs[k], b); err != nil {
						return err
					}
				}
			}
			if d.Attr&0x10 != 0 {
				if err := walk(d.First, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(f.rootc, 0); err != nil {
		return err
	}
	for sec := range touched {
		b := f.fat[sec*512 : (sec+1)*512]
		if err := f.writeSectors(f.reserved+sec, b); err != nil {
			return err
		}
		if err := f.writeSectors(f.reserved+f.fatsz+sec, b); err != nil {
			return err
		}
	}
	return nil
}
