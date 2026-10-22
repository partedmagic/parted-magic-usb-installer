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
	"bufio"
	"errors"
	"os"
	"strings"
	"sync/atomic"
)

var canceled atomic.Bool
var errCanceled = errors.New("canceled")

// startCancelWatcher reads stdin for the life of the run. A line "cancel"
// requests cancellation; EOF does too when the front end asks for it (the GUI
// closes the pipe). Plain CLI runs ignore EOF so scripts are not surprised.
func startCancelWatcher(cancelOnEOF bool) {
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if strings.TrimSpace(strings.ToLower(sc.Text())) == "cancel" {
				canceled.Store(true)
				return
			}
		}
		if cancelOnEOF {
			canceled.Store(true)
		}
	}()
}

func checkCanceled() error {
	if canceled.Load() {
		return errCanceled
	}
	return nil
}
