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

//go:build !linux && !windows && !darwin

package main

import "errors"

type Device struct {
	Path, Model, Vendor, Serial string
	Size                        int64
	Removable, ReadOnly, USB    bool
	SectorSize                  int
}

func listDevices(all, fixed bool, sourcePath string) ([]Device, error) {
	return nil, errors.New("device enumeration is not implemented on this OS yet; use a file target")
}

func openDevice(path, expect string, allowNonUSB, allowFixed bool, sourcePath string) (Target, *Device, error) {
	return nil, nil, errors.New("block device targets are not implemented on this OS yet")
}

func rereadPartitions(path string) error { return nil }

func isDevicePath(p string) bool { return false }

func openReadTarget(path string) (Target, error) { return openFileTargetRO(path) }
