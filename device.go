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
	"io"
	"os"
)

// Target is the raw destination: a regular file (testing, images) or a whole
// block device. The core never uses the host filesystem driver on it.
type Target interface {
	io.ReaderAt
	io.WriterAt
	Size() int64
	Sync() error
	Checkpoint() error // push what was written so far to the device (progress honesty)
	Close() error
	File() *os.File
	IsDevice() bool
}

// cacheSizer is implemented by targets with a block cache.
type cacheSizer interface{ SetCacheBlocks(n int) }

func cacheBlocksFor(fatBytes int64) int { return int(2*fatBytes/cacheBlockSize) + 32 }

type fileTarget struct {
	f    *os.File
	size int64
}

func (t *fileTarget) ReadAt(p []byte, off int64) (int, error)  { return t.f.ReadAt(p, off) }
func (t *fileTarget) WriteAt(p []byte, off int64) (int, error) { return t.f.WriteAt(p, off) }
func (t *fileTarget) Size() int64                              { return t.size }
func (t *fileTarget) Sync() error                              { return t.f.Sync() }
func (t *fileTarget) Checkpoint() error                        { return nil }
func (t *fileTarget) Close() error                             { return t.f.Close() }
func (t *fileTarget) File() *os.File                           { return t.f }
func (t *fileTarget) IsDevice() bool                           { return false }

// createFileTarget makes (or truncates) an image file of the given size.
func createFileTarget(path string, size int64) (*fileTarget, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return nil, err
	}
	return &fileTarget{f: f, size: size}, nil
}

// openFileTargetRO opens an existing image for verification.
func openFileTargetRO(path string) (*fileTarget, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &fileTarget{f: f, size: st.Size()}, nil
}

func zeroRange(t Target, off, n int64) error {
	buf := make([]byte, 1<<20)
	for n > 0 {
		c := int64(len(buf))
		if c > n {
			c = n
		}
		if _, err := t.WriteAt(buf[:c], off); err != nil {
			return fmt.Errorf("zero at %d: %w", off, err)
		}
		off += c
		n -= c
	}
	return nil
}
