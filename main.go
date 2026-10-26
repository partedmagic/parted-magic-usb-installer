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

// pmusb: writes Parted Magic to a USB drive (working name; rename freely).
// One static binary. Subcommands: inspect, install, verify, list-devices, version.
// Everything the design map (claude_output/usb_installer_design_2026-09-12.md)
// calls "the core" lives in this package.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

const version = "0.4"
const payloadName = "syslinux 6.04-20250811 (Debian syslinux 3:6.04~git20190206.bf6db5b4+dfsg1-3.2)"

var jsonOut bool

// guiHook is set by the Windows GUI build (tag "gui"); a bare launch opens the window.
var guiHook func()

// emit prints one progress event: JSON Lines with --json, plain text otherwise.
func emit(kind string, fields map[string]any) {
	if jsonOut {
		m := map[string]any{"event": kind}
		for k, v := range fields {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		fmt.Println(string(b))
		return
	}
	if kind == "progress" {
		return // text mode: phases, files and messages only
	}
	if kind == "file" {
		// text mode: name only the big files, so a run is readable
		if sz, ok := fields["size"].(int64); ok && sz >= 64<<20 {
			fmt.Fprintf(os.Stderr, "  %s %v\n", fields["phase"], fields["path"])
		}
		return
	}
	var sb strings.Builder
	sb.WriteString(kind)
	for _, k := range []string{"phase", "message", "path", "done", "total", "code"} {
		if v, ok := fields[k]; ok {
			fmt.Fprintf(&sb, " %s=%v", k, v)
		}
	}
	fmt.Fprintln(os.Stderr, sb.String())
}

func fail(code int, phase, msg string) {
	emit("error", map[string]any{"phase": phase, "code": code, "message": msg})
	os.Exit(code)
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  pmusb inspect --source ISO|DIR
  pmusb install --source ISO|DIR --target DEV|FILE --confirm-erase [--expect "model|serial|bytes"]
                [--size BYTES] [--label PMAGIC] [--volume-id HEX] [--no-verify] [--no-bios]
                [--allow-module-mismatch] [--json]
  pmusb verify  --target DEV|FILE
  pmusb upgrade --source ISO|DIR --target DEV|FILE --confirm-upgrade [--delete-session] [--no-verify]
  pmusb list-devices [--all] [--fixed] [--json]
  pmusb version
exit codes: 0 ok, 2 usage, 3 refused by a safety check, 4 device error, 5 verify failed, 6 canceled
`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		if guiHook != nil {
			guiHook()
			return
		}
		usage()
	}
	switch os.Args[1] {
	case "version":
		fmt.Printf("pmusb %s, boot payload %s\n", version, payloadName)
	case "inspect":
		fs := flag.NewFlagSet("inspect", flag.ExitOnError)
		src := fs.String("source", "", "ISO file or extracted tree")
		fs.BoolVar(&jsonOut, "json", false, "JSON Lines output")
		fs.Parse(os.Args[2:])
		if *src == "" {
			usage()
		}
		s, err := openSource(*src)
		if err != nil {
			fail(3, "source", err.Error())
		}
		defer s.Close()
		if err := s.validate(); err != nil {
			fail(3, "source", err.Error())
		}
		b, _ := json.MarshalIndent(s.summary(), "", "  ")
		fmt.Println(string(b))
	case "install":
		cmdInstall(os.Args[2:])
	case "verify":
		cmdVerify(os.Args[2:])
	case "upgrade":
		cmdUpgrade(os.Args[2:])
	case "list-devices":
		fs := flag.NewFlagSet("list-devices", flag.ExitOnError)
		all := fs.Bool("all", false, "list non-USB disks too")
		fixed := fs.Bool("fixed", false, "list USB devices that report fixed media")
		fs.BoolVar(&jsonOut, "json", false, "JSON Lines output")
		fs.Parse(os.Args[2:])
		devs, err := listDevices(*all, *fixed, "")
		if err != nil {
			fail(4, "devices", err.Error())
		}
		if jsonOut {
			b, _ := json.Marshal(map[string]any{"event": "devices", "devices": devs})
			fmt.Println(string(b))
			return
		}
		for _, d := range devs {
			fmt.Printf("%s\t%s\t%d bytes\t%s\tremovable=%v\tsector=%d\n", d.Path, d.Model, d.Size, d.Serial, d.Removable, d.SectorSize)
		}
	default:
		usage()
	}
}
