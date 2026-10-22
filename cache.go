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
	"fmt"
	"unsafe"
)

// blockCache is the write-back cache the device layers share: 16 aligned
// 1 MiB blocks between the byte-granular callers (go-diskfs, the FAT walker,
// the verifier) and a raw handle that only accepts sector-aligned transfers
// (Windows NO_BUFFERING, Linux O_DIRECT). Sequential file data reaches the
// drive as 1 MiB writes; FAT and directory updates coalesce in their blocks.
const (
	cacheBlockSize = 1 << 20
	cacheBlocks    = 16
	alignment      = 4096
)

type rawDevice interface {
	rawRead(b []byte, off int64) error
	rawWrite(b []byte, off int64) error
}

type cacheBlock struct {
	off   int64
	buf   []byte
	dirty bool
	used  uint64
}

type blockCache struct {
	dev    rawDevice
	size   int64
	blocks []*cacheBlock
	tick   uint64
	max    int // block count; grown by SetCacheBlocks once the FAT size is known
}

// SetCacheBlocks makes room for both FATs plus a streaming margin: go-diskfs
// rewrites the whole FAT tables on every file, and a cache smaller than the
// FATs flushes them to the device again and again (measured: 35x write
// amplification on a 30 GB stick).
func (c *blockCache) SetCacheBlocks(n int) {
	if n < cacheBlocks {
		n = cacheBlocks
	}
	if n > 1024 {
		n = 1024
	}
	c.max = n
}

func alignedBuffer(n int) []byte {
	b := make([]byte, n+alignment)
	off := int(alignment-uintptr(unsafe.Pointer(&b[0]))%alignment) % alignment
	return b[off : off+n]
}

func (c *blockCache) blockLen(off int64) int {
	n := int64(cacheBlockSize)
	if off+n > c.size {
		n = c.size - off
	}
	return int(n)
}

// block returns the cached block at the aligned offset, loading it from the
// device unless the caller is about to overwrite all of it.
func (c *blockCache) block(off int64, wholeWrite bool) (*cacheBlock, error) {
	c.tick++
	for _, b := range c.blocks {
		if b.off == off {
			b.used = c.tick
			return b, nil
		}
	}
	var victim *cacheBlock
	limit := c.max
	if limit == 0 {
		limit = cacheBlocks
	}
	if len(c.blocks) < limit {
		victim = &cacheBlock{off: -1, buf: alignedBuffer(cacheBlockSize)}
		c.blocks = append(c.blocks, victim)
	} else {
		for _, b := range c.blocks {
			if victim == nil || b.used < victim.used {
				victim = b
			}
		}
		if err := c.flushBlock(victim); err != nil {
			return nil, err
		}
	}
	victim.off, victim.used, victim.dirty = off, c.tick, false
	if !wholeWrite {
		if err := c.dev.rawRead(victim.buf[:c.blockLen(off)], off); err != nil {
			victim.off = -1
			return nil, err
		}
	}
	return victim, nil
}

func (c *blockCache) flushBlock(b *cacheBlock) error {
	if !b.dirty || b.off < 0 {
		return nil
	}
	if err := c.dev.rawWrite(b.buf[:c.blockLen(b.off)], b.off); err != nil {
		return err
	}
	b.dirty = false
	return nil
}

func (c *blockCache) flushAll() error {
	for _, b := range c.blocks {
		if err := c.flushBlock(b); err != nil {
			return err
		}
	}
	return nil
}

func (c *blockCache) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > c.size {
		return 0, fmt.Errorf("read beyond the end of the device")
	}
	n := 0
	for n < len(p) {
		cur := off + int64(n)
		boff := cur &^ (cacheBlockSize - 1)
		in := int(cur - boff)
		chunk := cacheBlockSize - in
		if chunk > len(p)-n {
			chunk = len(p) - n
		}
		b, err := c.block(boff, false)
		if err != nil {
			return n, err
		}
		copy(p[n:n+chunk], b.buf[in:in+chunk])
		n += chunk
	}
	return n, nil
}

func (c *blockCache) WriteAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > c.size {
		return 0, fmt.Errorf("write beyond the end of the device")
	}
	n := 0
	for n < len(p) {
		cur := off + int64(n)
		boff := cur &^ (cacheBlockSize - 1)
		in := int(cur - boff)
		chunk := cacheBlockSize - in
		if chunk > len(p)-n {
			chunk = len(p) - n
		}
		b, err := c.block(boff, in == 0 && chunk == c.blockLen(boff))
		if err != nil {
			return n, err
		}
		copy(b.buf[in:in+chunk], p[n:n+chunk])
		b.dirty = true
		n += chunk
	}
	return n, nil
}
