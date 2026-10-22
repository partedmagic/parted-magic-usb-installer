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
	"errors"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/diskfs/go-diskfs/backend"
)

// targetStorage adapts a Target to go-diskfs's backend.Storage so the
// populator writes through the core's device layer (aligned I/O on Windows).
type targetStorage struct {
	t   Target
	pos int64
}

type targetInfo struct{ size int64 }

func (i targetInfo) Name() string       { return "target" }
func (i targetInfo) Size() int64        { return i.size }
func (i targetInfo) Mode() fs.FileMode  { return 0600 }
func (i targetInfo) ModTime() time.Time { return time.Time{} }
func (i targetInfo) IsDir() bool        { return false }
func (i targetInfo) Sys() any           { return nil }

func (s *targetStorage) Stat() (fs.FileInfo, error) { return targetInfo{s.t.Size()}, nil }
func (s *targetStorage) Read(p []byte) (int, error) {
	n, err := s.t.ReadAt(p, s.pos)
	s.pos += int64(n)
	return n, err
}
func (s *targetStorage) ReadAt(p []byte, off int64) (int, error)  { return s.t.ReadAt(p, off) }
func (s *targetStorage) WriteAt(p []byte, off int64) (int, error) { return s.t.WriteAt(p, off) }
func (s *targetStorage) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		s.pos = off
	case io.SeekCurrent:
		s.pos += off
	case io.SeekEnd:
		s.pos = s.t.Size() + off
	default:
		return 0, errors.New("bad whence")
	}
	return s.pos, nil
}
func (s *targetStorage) Close() error                            { return nil }
func (s *targetStorage) Sys() (*os.File, error)                  { return s.t.File(), nil }
func (s *targetStorage) Writable() (backend.WritableFile, error) { return s, nil }
func (s *targetStorage) Path() string                            { return "" }
