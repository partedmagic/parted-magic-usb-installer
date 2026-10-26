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

//go:build windows && gui

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
)

// The Windows window. It never touches a disk itself: it runs this same
// (already elevated) executable as a child with --json and reads its events,
// exactly like the GTK front end does on Linux.

func init() { guiHook = runGUI }

type gui struct {
	mw                               *walk.MainWindow
	isoEdit                          *walk.LineEdit
	isoStatus, status                *walk.Label
	combo                            *walk.ComboBox
	verifyCB, biosCB, fixedCB, allCB *walk.CheckBox
	installBtn, cancelBtn            *walk.PushButton
	fileLabel, overallLabel          *walk.Label
	fileBar, bar                     *walk.ProgressBar
	logEdit                          *walk.TextEdit
	exe                              string
	settings                         map[string]any
	devices                          []Device
	info                             *summary
	proc                             *exec.Cmd
	stdin                            io.WriteCloser
	finished                         bool
	result                           string
}

func humanBytes(n int64) string {
	f := float64(n)
	for _, u := range []string{"bytes", "KB", "MB", "GB", "TB"} {
		if f < 1000 || u == "TB" {
			if u == "bytes" {
				return fmt.Sprintf("%d bytes", n)
			}
			return fmt.Sprintf("%.1f %s", f, u)
		}
		f /= 1000
	}
	return ""
}

// isoName is what the window calls the ISO: "Parted Magic 26.10", else its
// volume label, else its file name.
func isoName(s *summary) string {
	switch {
	case s.Version != "":
		return "Parted Magic " + s.Version
	case s.Label != "":
		return s.Label
	}
	return filepath.Base(s.Source)
}

func runGUI() {
	g := &gui{}
	g.exe, _ = os.Executable()
	err := MainWindow{
		AssignTo: &g.mw, Title: "Parted Magic USB Installer",
		MinSize: Size{Width: 560, Height: 440}, Size: Size{Width: 620, Height: 480},
		Layout: VBox{},
		Children: []Widget{
			Composite{Layout: Grid{Columns: 3}, Children: []Widget{
				Label{Text: "Parted Magic ISO"},
				LineEdit{AssignTo: &g.isoEdit, ReadOnly: true},
				PushButton{Text: "Browse...", OnClicked: g.browse},
				Label{Text: ""},
				Label{AssignTo: &g.isoStatus, Text: "", ColumnSpan: 2},
				Label{Text: "USB drive"},
				ComboBox{AssignTo: &g.combo, OnCurrentIndexChanged: g.updateReady},
				PushButton{Text: "Refresh", OnClicked: g.refresh},
			}},
			CheckBox{AssignTo: &g.verifyCB, Alignment: AlignHNearVCenter, Text: "Verify after writing (recommended)", Checked: true},
			CheckBox{AssignTo: &g.biosCB, Alignment: AlignHNearVCenter, Text: "Include legacy BIOS boot loader", Checked: true},
			CheckBox{AssignTo: &g.fixedCB, Alignment: AlignHNearVCenter, Text: "Show USB hard drives", OnCheckedChanged: g.refresh},
			CheckBox{AssignTo: &g.allCB, Alignment: AlignHNearVCenter, Text: "Show all drives (not only USB)", OnCheckedChanged: g.refresh},
			Composite{Layout: HBox{}, Children: []Widget{
				HSpacer{},
				PushButton{AssignTo: &g.cancelBtn, Text: "Cancel", Enabled: false, OnClicked: g.cancel},
				PushButton{AssignTo: &g.installBtn, Text: "Install", Enabled: false, OnClicked: g.install},
			}},
			Label{AssignTo: &g.status, Text: ""},
			Label{AssignTo: &g.fileLabel, Text: ""},
			ProgressBar{AssignTo: &g.fileBar, MaxValue: 1000},
			Label{AssignTo: &g.overallLabel, Text: ""},
			ProgressBar{AssignTo: &g.bar, MaxValue: 1000},
			TextEdit{AssignTo: &g.logEdit, ReadOnly: true, VScroll: true, MinSize: Size{Height: 120}},
		},
	}.Create()
	if err != nil {
		walk.MsgBox(nil, "Parted Magic USB Installer", err.Error(), walk.MsgBoxIconError)
		return
	}
	if ic, err := walk.NewIconFromResource("APP"); err == nil {
		g.mw.SetIcon(ic)
	}
	g.loadSettings()
	g.mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) { g.saveSettings() })
	g.refresh()
	g.mw.Run()
}

func settingsPath() string {
	d := os.Getenv("APPDATA")
	if d == "" {
		return ""
	}
	os.MkdirAll(filepath.Join(d, "pmusb"), 0700)
	return filepath.Join(d, "pmusb", "settings.json")
}

func (g *gui) loadSettings() {
	g.settings = map[string]any{}
	if p := settingsPath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			json.Unmarshal(b, &g.settings)
		}
	}
	if v, ok := g.settings["verify"].(bool); ok {
		g.verifyCB.SetChecked(v)
	}
	if v, ok := g.settings["bios"].(bool); ok {
		g.biosCB.SetChecked(v)
	}
}

func (g *gui) saveSettings() {
	g.settings["verify"] = g.verifyCB.Checked()
	g.settings["bios"] = g.biosCB.Checked()
	if p := settingsPath(); p != "" {
		if b, err := json.MarshalIndent(g.settings, "", "  "); err == nil {
			os.WriteFile(p, b, 0600)
		}
	}
}

func (g *gui) runJSON(args ...string) ([]map[string]any, string, int) {
	cmd := exec.Command(g.exe, append(args, "--json")...)
	out, err := cmd.CombinedOutput()
	rc := 0
	if ee, ok := err.(*exec.ExitError); ok {
		rc = ee.ExitCode()
	} else if err != nil {
		rc = 1
	}
	var events []map[string]any
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") {
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) == nil {
				events = append(events, m)
			}
		}
	}
	return events, string(out), rc
}

func (g *gui) refresh() {
	args := []string{"list-devices"}
	if g.allCB != nil && g.allCB.Checked() {
		args = append(args, "--all")
	}
	if g.fixedCB != nil && g.fixedCB.Checked() {
		args = append(args, "--fixed")
	}
	events, _, _ := g.runJSON(args...)
	g.devices = nil
	for _, e := range events {
		if e["event"] == "devices" {
			b, _ := json.Marshal(e["devices"])
			json.Unmarshal(b, &g.devices)
		}
	}
	var items []string
	for _, d := range g.devices {
		kind := "USB"
		if !d.USB {
			kind = "not USB"
		}
		items = append(items, fmt.Sprintf("%s, %s, %s, Disk %d %s", d.Model, humanBytes(d.Size), kind, d.Number, d.Letters))
	}
	if len(items) == 0 {
		items = []string{"No USB drive found. Plug one in and click Refresh."}
	}
	g.combo.SetModel(items)
	g.combo.SetCurrentIndex(0)
	g.updateReady()
}

func (g *gui) browse() {
	dlg := new(walk.FileDialog)
	dlg.Title = "Choose the Parted Magic ISO"
	dlg.Filter = "ISO images (*.iso)|*.iso|All files (*.*)|*.*"
	if d, ok := g.settings["last_iso_dir"].(string); ok {
		dlg.InitialDirPath = d
	}
	if ok, err := dlg.ShowOpen(g.mw); err != nil || !ok {
		return
	}
	g.isoEdit.SetText(dlg.FilePath)
	g.settings["last_iso_dir"] = filepath.Dir(dlg.FilePath)
	g.saveSettings()
	g.info = nil
	events, raw, rc := g.runJSON("inspect", "--source", dlg.FilePath)
	var s summary
	g.biosCB.SetEnabled(true)
	if rc == 0 && json.Unmarshal([]byte(strings.TrimSpace(raw)), &s) == nil {
		g.info = &s
		build := ""
		if s.Build != "" {
			build = ", build " + s.Build
		}
		txt := fmt.Sprintf("%s%s, %s", isoName(&s), build, humanBytes(s.Bytes))
		if s.BIOSLoader == "" {
			// No BIOS boot loader on the ISO (the arm64 one): the core makes
			// a UEFI-only stick whatever the box says, so the box is greyed.
			txt += " (UEFI only)"
			g.biosCB.SetEnabled(false)
		} else if !s.BIOSModulesMatch {
			txt += " (BIOS modules newer than this installer: UEFI-only)"
			g.biosCB.SetChecked(false)
		}
		g.isoStatus.SetText(txt)
	} else {
		msg := "Not a Parted Magic ISO"
		for _, e := range events {
			if m, ok := e["message"].(string); ok {
				msg = m
			}
		}
		g.isoStatus.SetText(msg)
	}
	g.updateReady()
}

func (g *gui) selected() *Device {
	i := g.combo.CurrentIndex()
	if i < 0 || i >= len(g.devices) {
		return nil
	}
	return &g.devices[i]
}

func (g *gui) updateReady() {
	d := g.selected()
	ok := g.info != nil && d != nil && g.proc == nil
	if ok && d.Size < g.info.RequiredBytes {
		ok = false
		g.status.SetText(fmt.Sprintf("Drive too small: needs %s", humanBytes(g.info.RequiredBytes)))
	}
	g.installBtn.SetEnabled(ok)
}

func (g *gui) install() {
	d := g.selected()
	if d == nil || g.info == nil {
		return
	}
	kind := "USB"
	if !d.USB {
		kind = "not USB"
	}
	build := ""
	if g.info.Build != "" {
		build = fmt.Sprintf(" (build %s)", g.info.Build)
	}
	text := fmt.Sprintf("Everything on this drive will be erased.\r\n\r\n%s, %s, %s, serial %s\r\nDisk %d %s\r\n\r\n%s%s will be installed from:\r\n%s\r\n\r\nThis cannot be undone.",
		d.Model, humanBytes(d.Size), kind, d.Serial, d.Number, d.Letters, isoName(g.info), build, g.isoEdit.Text())
	if d.USB && !d.Removable {
		text += "\r\n\r\nThis device reports itself as a hard drive. Parted Magic will take about 10 seconds longer to find it at boot."
	}
	if walk.MsgBox(g.mw, "Erase and install?", text, walk.MsgBoxIconWarning|walk.MsgBoxOKCancel|walk.MsgBoxDefButton2) != walk.DlgCmdOK {
		return
	}
	args := []string{"install", "--source", g.isoEdit.Text(), "--target", d.Path, "--confirm-erase",
		"--expect", fmt.Sprintf("%s|%s|%d", d.Model, d.Serial, d.Size), "--json", "--cancel-on-eof"}
	if !g.verifyCB.Checked() {
		args = append(args, "--no-verify")
	}
	if !g.biosCB.Checked() {
		args = append(args, "--no-bios")
	}
	if g.allCB.Checked() {
		args = append(args, "--allow-non-usb")
	}
	if g.fixedCB.Checked() {
		args = append(args, "--allow-fixed")
	}
	cmd := exec.Command(g.exe, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		g.status.SetText(err.Error())
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		g.status.SetText(err.Error())
		return
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		g.status.SetText("Could not start the installer: " + err.Error())
		return
	}
	g.proc, g.stdin, g.finished = cmd, stdin, false
	g.logEdit.SetText("")
	g.status.SetText("Starting...")
	g.fileLabel.SetText("")
	g.overallLabel.SetText("")
	g.bar.SetValue(0)
	g.fileBar.SetValue(0)
	g.installBtn.SetEnabled(false)
	g.cancelBtn.SetEnabled(true)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			g.mw.Synchronize(func() { g.onEvent(line) })
		}
		rc := 0
		if err := cmd.Wait(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				rc = ee.ExitCode()
			} else {
				rc = 1
			}
		}
		g.mw.Synchronize(func() { g.onExit(rc) })
	}()
}

func (g *gui) onEvent(line string) {
	var e map[string]any
	if json.Unmarshal([]byte(line), &e) != nil {
		g.logEdit.AppendText(line + "\r\n")
		return
	}
	names := map[string]string{"source": "Reading the ISO", "prepare": "Preparing the drive", "format": "Formatting", "copy": "Writing files",
		"bootloader": "Installing boot loader", "flush": "Flushing", "verify": "Verifying", "finish": "Finishing"}
	verb := map[string]string{"copy": "Copying", "verify": "Verifying"}
	ph, _ := e["phase"].(string)
	switch e["event"] {
	case "phase":
		g.status.SetText(names[ph])
		g.logEdit.AppendText("== " + names[ph] + "\r\n")
		if ph == "copy" || ph == "verify" {
			g.bar.SetValue(0)
			g.fileBar.SetValue(0)
		}
	case "file":
		path, _ := e["path"].(string)
		size, _ := e["size"].(float64)
		g.logEdit.AppendText(fmt.Sprintf("%s %s (%s)\r\n", verb[ph], path, humanBytes(int64(size))))
		g.fileLabel.SetText(fmt.Sprintf("%s %s", verb[ph], path))
		g.fileBar.SetValue(0)
	case "progress":
		done, _ := e["done"].(float64)
		total, _ := e["total"].(float64)
		fd, _ := e["file_done"].(float64)
		ft, _ := e["file_total"].(float64)
		path, _ := e["path"].(string)
		if ft > 0 {
			g.fileBar.SetValue(int(fd / ft * 1000))
			g.fileLabel.SetText(fmt.Sprintf("%s %s (%s of %s)", verb[ph], path, humanBytes(int64(fd)), humanBytes(int64(ft))))
		}
		if total > 0 {
			g.bar.SetValue(int(done / total * 1000))
			g.overallLabel.SetText(fmt.Sprintf("Overall: %d%% (%s)", int(done/total*100), strings.ToLower(names[ph])))
		}
	case "log":
		msg, _ := e["message"].(string)
		g.logEdit.AppendText(msg + "\r\n")
	case "error":
		msg, _ := e["message"].(string)
		g.status.SetText(msg)
		g.logEdit.AppendText("ERROR: " + msg + "\r\n")
		g.finished = true
		g.result = "error: " + msg
	case "canceled":
		g.status.SetText("Canceled. The drive has no partition table; run the installer again to use it.")
		g.finished = true
		g.result = "canceled"
	case "done":
		g.bar.SetValue(1000)
		note := " Verification passed."
		if v, ok := e["verified"].(bool); ok && !v {
			note = " Verification skipped."
		}
		msg, _ := e["message"].(string)
		g.status.SetText("Done. " + msg + "." + note)
		g.overallLabel.SetText("Overall: 100%")
		g.finished = true
		g.result = "done: " + msg + "." + note + " Use Safely Remove Hardware, or unplug and reconnect the drive to see it in Explorer."
	}
}

func (g *gui) onExit(rc int) {
	g.proc, g.stdin = nil, nil
	g.cancelBtn.SetEnabled(false)
	if !g.finished {
		g.status.SetText(fmt.Sprintf("The installer stopped with exit code %d. See the log below.", rc))
		g.result = fmt.Sprintf("error: the installer stopped with exit code %d. See the log.", rc)
	}
	g.refresh()
	switch {
	case strings.HasPrefix(g.result, "done: "):
		walk.MsgBox(g.mw, "Parted Magic is on the drive", strings.TrimPrefix(g.result, "done: "), walk.MsgBoxIconInformation)
	case g.result == "canceled":
		walk.MsgBox(g.mw, "Canceled", "The drive has no partition table. Run the installer again to use it.", walk.MsgBoxIconWarning)
	default:
		walk.MsgBox(g.mw, "The installer failed", strings.TrimPrefix(g.result, "error: "), walk.MsgBoxIconError)
	}
}

func (g *gui) cancel() {
	if g.stdin != nil {
		g.stdin.Close()
		g.status.SetText("Canceling after the current write...")
	}
}
