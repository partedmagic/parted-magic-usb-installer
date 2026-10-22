#!/usr/bin/env python3
# Parted Magic USB installer (working name pmusb), GTK 3 window.
#
# Copyright 2026 Patrick J. Verner, Waupaca, WI, USA
#
# This program is free software; you can redistribute it and/or modify it
# under the terms of the GNU General Public License as published by the Free
# Software Foundation; either version 2 of the License, or (at your option)
# any later version.
#
# This program is distributed in the hope that it will be useful, but WITHOUT
# ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
# FITNESS FOR A PARTICULAR PURPOSE. See the GNU General Public License for
# more details. You should have received a copy of the GNU General Public
# License along with this program; if not, see <https://www.gnu.org/licenses/>.

# Parted Magic USB installer, GTK 3 front end for Linux and for the Parted Magic
# image itself. Working name pmusb. Python 3 standard library plus PyGObject.
# The disk work is done by the pmusb core beside this file (or on PATH), run
# under pkexec unless already root; this window never touches a disk.
import json, os, shutil, subprocess, threading, time
import gi
gi.require_version("Gtk", "3.0")
from gi.repository import Gtk, GLib

APP_TITLE = "Parted Magic USB Installer"
HERE = os.path.dirname(os.path.abspath(__file__))
PHASES = {"source": "Reading the ISO", "prepare": "Preparing the drive", "format": "Formatting",
          "copy": "Writing files", "bootloader": "Installing boot loader", "flush": "Flushing to the drive",
          "verify": "Verifying", "finish": "Finishing"}
VERB = {"copy": "Copying", "verify": "Verifying"}

def find_core():
    for c in (os.path.join(HERE, "pmusb"), shutil.which("pmusb")):
        if c and os.access(c, os.X_OK):
            return c
    return None

def human(n):
    n = float(n)
    for unit in ("bytes", "KB", "MB", "GB", "TB"):
        if n < 1000 or unit == "TB":
            return f"{int(n)} bytes" if unit == "bytes" else f"{n:.1f} {unit}"
        n /= 1000.0

def settings_path():
    d = os.path.join(GLib.get_user_config_dir(), "pmusb")
    os.makedirs(d, exist_ok=True)
    return os.path.join(d, "settings.json")

class App(Gtk.Window):
    def __init__(self):
        super().__init__(title=APP_TITLE)
        self.core = find_core()
        self.proc = None
        self.devices = []
        self.source_ok = None
        self.settings = {}
        try:
            with open(settings_path()) as f:
                self.settings = json.load(f)
        except (OSError, ValueError):
            pass
        icon = os.path.join(HERE, "pmusb.png")
        if os.path.exists(icon):
            self.set_icon_from_file(icon)
        else:
            self.set_icon_name("pmagic")
        self.set_border_width(12)
        self.set_resizable(True)
        grid = Gtk.Grid(column_spacing=8, row_spacing=6)
        self.add(grid)
        row = 0
        grid.attach(Gtk.Label(label="Parted Magic ISO", xalign=0), 0, row, 1, 1)
        self.iso = Gtk.FileChooserButton(title="Choose the Parted Magic ISO")
        self.iso.set_hexpand(True)
        f = Gtk.FileFilter(); f.set_name("ISO images"); f.add_pattern("*.iso"); f.add_pattern("*.ISO")
        self.iso.add_filter(f)
        if self.settings.get("last_iso_dir") and os.path.isdir(self.settings["last_iso_dir"]):
            self.iso.set_current_folder(self.settings["last_iso_dir"])
        self.iso.connect("file-set", self.on_iso)
        grid.attach(self.iso, 1, row, 2, 1); row += 1
        self.iso_status = Gtk.Label(label="", xalign=0); self.iso_status.set_line_wrap(True)
        grid.attach(self.iso_status, 1, row, 2, 1); row += 1
        grid.attach(Gtk.Label(label="USB drive", xalign=0), 0, row, 1, 1)
        self.combo = Gtk.ComboBoxText(); self.combo.set_hexpand(True)
        grid.attach(self.combo, 1, row, 1, 1)
        refresh = Gtk.Button(label="Refresh"); refresh.connect("clicked", lambda *_: self.refresh())
        grid.attach(refresh, 2, row, 1, 1); row += 1
        adv = Gtk.Expander(label="Advanced")
        box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=2)
        lb = Gtk.Box(spacing=6); lb.pack_start(Gtk.Label(label="Volume label"), False, False, 0)
        self.label = Gtk.Entry(text=self.settings.get("label", "PMAGIC"), max_length=11, width_chars=12)
        lb.pack_start(self.label, False, False, 0)
        box.pack_start(lb, False, False, 0)
        def check(text, key, default, tip=None):
            cb = Gtk.CheckButton(label=text)
            cb.set_halign(Gtk.Align.START)   # clickable area ends with the text
            cb.set_active(bool(self.settings.get(key, default)))
            if tip:
                cb.set_tooltip_text(tip)
            box.pack_start(cb, False, False, 0)
            return cb
        self.verify = check("Verify after writing (recommended)", "verify", True)
        self.bios = check("Include legacy BIOS boot loader (syslinux)", "bios", True)
        self.opt_fixed = check("Show USB hard drives", "show_fixed", False,
                               "USB devices that report fixed media, such as SSD enclosures, are hidden by default.")
        self.opt_all = check("Show all drives (not only USB)", "show_all", False,
                             "Also lists non-USB disks. Disks holding this system, disks with mounted filesystems, "
                             "and the disk holding the ISO are never offered.")
        self.opt_fixed.connect("toggled", lambda *_: (self.save_settings(), self.refresh()))
        self.opt_all.connect("toggled", lambda *_: (self.save_settings(), self.refresh()))
        self.verify.connect("toggled", lambda *_: self.save_settings())
        self.bios.connect("toggled", lambda *_: self.save_settings())
        adv.add(box)
        grid.attach(adv, 0, row, 3, 1); row += 1
        self.install = Gtk.Button(label="Install"); self.install.connect("clicked", self.on_install)
        self.install.set_sensitive(False)
        self.cancel = Gtk.Button(label="Cancel"); self.cancel.connect("clicked", self.on_cancel); self.cancel.set_sensitive(False)
        bb = Gtk.Box(spacing=8); bb.pack_end(self.install, False, False, 0); bb.pack_end(self.cancel, False, False, 0)
        grid.attach(bb, 0, row, 3, 1); row += 1
        self.phase = Gtk.Label(label="", xalign=0); self.phase.set_line_wrap(True)
        grid.attach(self.phase, 0, row, 3, 1); row += 1
        self.file_label = Gtk.Label(label="", xalign=0); self.file_label.set_ellipsize(3)  # END
        grid.attach(self.file_label, 0, row, 3, 1); row += 1
        self.file_bar = Gtk.ProgressBar(); grid.attach(self.file_bar, 0, row, 3, 1); row += 1
        self.overall_label = Gtk.Label(label="", xalign=0); grid.attach(self.overall_label, 0, row, 3, 1); row += 1
        self.bar = Gtk.ProgressBar(); grid.attach(self.bar, 0, row, 3, 1); row += 1
        det = Gtk.Expander(label="Details"); det.set_resize_toplevel(True)
        sw = Gtk.ScrolledWindow(); sw.set_min_content_height(160); sw.set_vexpand(True)
        self.log = Gtk.TextView(editable=False, monospace=True, cursor_visible=False); sw.add(self.log); det.add(sw)
        grid.attach(det, 0, row, 3, 1); row += 1
        self.combo.connect("changed", lambda *_: self.update_ready())
        self.connect("destroy", self.on_destroy)
        if not self.core:
            self.iso_status.set_text("The pmusb core program was not found beside this script or on PATH.")
        self.refresh()
        if self.settings.get("last_iso") and os.path.isfile(self.settings["last_iso"]):
            self.iso.set_filename(self.settings["last_iso"])
            GLib.idle_add(self.on_iso)

    def save_settings(self):
        self.settings.update({"verify": self.verify.get_active(), "bios": self.bios.get_active(),
                              "show_fixed": self.opt_fixed.get_active(), "show_all": self.opt_all.get_active(),
                              "label": self.label.get_text()})
        try:
            with open(settings_path(), "w") as f:
                json.dump(self.settings, f, indent=2)
        except OSError:
            pass

    def on_destroy(self, *_):
        self.save_settings()
        Gtk.main_quit()

    def logline(self, s):
        b = self.log.get_buffer(); b.insert(b.get_end_iter(), s + "\n")
        self.log.scroll_to_mark(b.create_mark(None, b.get_end_iter(), False), 0.0, False, 0.0, 0.0)

    def dialog(self, kind, title, text):
        dlg = Gtk.MessageDialog(transient_for=self, modal=True, message_type=kind, buttons=Gtk.ButtonsType.OK, text=title)
        dlg.format_secondary_text(text)
        dlg.run(); dlg.destroy()

    def run_json(self, args):
        if not self.core:
            return [], 1
        p = subprocess.run([self.core] + args + ["--json"], capture_output=True, text=True)
        events = []
        for line in (p.stdout + "\n" + p.stderr).splitlines():
            line = line.strip()
            if line.startswith("{"):
                try:
                    events.append(json.loads(line))
                except ValueError:
                    pass
        return events, p.returncode

    def refresh(self):
        args = ["list-devices"]
        if self.opt_all.get_active(): args.append("--all")
        if self.opt_fixed.get_active(): args.append("--fixed")
        events, rc = self.run_json(args)
        self.devices = []
        for e in events:
            if e.get("event") == "devices":
                self.devices = e.get("devices") or []
        self.combo.remove_all()
        for d in self.devices:
            kind = "USB" if d.get("USB") else "not USB"
            self.combo.append_text(f"{d['Model']}, {human(d['Size'])}, {kind}, {d['Path']}")
        if self.devices:
            self.combo.set_active(0)
        else:
            self.combo.append_text("No USB drive found. Plug one in and click Refresh."); self.combo.set_active(0)
        self.update_ready()

    def on_iso(self, *_):
        path = self.iso.get_filename()
        self.source_ok = None
        if not path or not self.core:
            self.update_ready(); return
        self.settings["last_iso"] = path; self.settings["last_iso_dir"] = os.path.dirname(path); self.save_settings()
        p = subprocess.run([self.core, "inspect", "--source", path, "--json"], capture_output=True, text=True)
        try:
            info = json.loads(p.stdout)
        except ValueError:
            info = None
        if p.returncode != 0 or not info:
            msg = "Not a Parted Magic ISO"
            for line in p.stdout.splitlines() + p.stderr.splitlines():
                if line.startswith("{"):
                    try:
                        msg = json.loads(line).get("message", msg)
                    except ValueError:
                        pass
            self.iso_status.set_markup(f'<span foreground="red">{GLib.markup_escape_text(msg)}</span>')
        else:
            self.source_ok = info
            build = f", build {info['Build']}" if info.get("Build") else ""
            txt = f"Parted Magic {info['Version']}{build}, {human(info['Bytes'])}"
            if not info.get("BIOSModulesMatch"):
                txt += " (BIOS modules newer than this installer: UEFI-only)"
                self.bios.set_active(False)
            self.iso_status.set_text(txt)
        self.update_ready()

    def selected_device(self):
        i = self.combo.get_active()
        return self.devices[i] if 0 <= i < len(self.devices) else None

    def update_ready(self):
        d = self.selected_device()
        ok = bool(self.source_ok and d and self.proc is None)
        if ok and d["Size"] < self.source_ok["RequiredBytes"]:
            ok = False
            self.install.set_tooltip_text(f"Drive too small: needs {human(self.source_ok['RequiredBytes'])}")
        else:
            self.install.set_tooltip_text(None)
        self.install.set_sensitive(ok)

    def on_install(self, *_):
        d = self.selected_device(); info = self.source_ok
        if not d or not info:
            return
        text = ("Everything on this drive will be erased.\n\n"
                f"{d['Model']}, {human(d['Size'])}, {'USB' if d.get('USB') else 'not USB'}, serial {d.get('Serial') or 'unknown'}\n"
                f"Device {d['Path']}\n\n"
                f"Parted Magic {info['Version']}" + (f" (build {info['Build']})" if info.get('Build') else "") +
                f" will be installed from:\n{self.iso.get_filename()}\n\nThis cannot be undone.")
        if d.get("USB") and not d.get("Removable"):
            text += "\n\nThis device reports itself as a hard drive. Parted Magic will take about 10 seconds longer to find it at boot."
        dlg = Gtk.MessageDialog(transient_for=self, modal=True, message_type=Gtk.MessageType.WARNING,
                                buttons=Gtk.ButtonsType.NONE, text=text)
        dlg.add_button("Cancel", Gtk.ResponseType.CANCEL)
        dlg.add_button("Erase and Install", Gtk.ResponseType.OK)
        dlg.set_default_response(Gtk.ResponseType.CANCEL)
        r = dlg.run(); dlg.destroy()
        if r != Gtk.ResponseType.OK:
            return
        self.save_settings()
        args = [self.core, "install", "--source", self.iso.get_filename(), "--target", d["Path"], "--confirm-erase",
                "--expect", f"{d['Model']}|{d.get('Serial','')}|{d['Size']}", "--label", self.label.get_text() or "PMAGIC",
                "--json", "--cancel-on-eof"]
        if not self.verify.get_active(): args.append("--no-verify")
        if not self.bios.get_active(): args.append("--no-bios")
        if self.opt_all.get_active(): args.append("--allow-non-usb")
        if self.opt_fixed.get_active(): args.append("--allow-fixed")
        if os.geteuid() != 0:
            args = ["pkexec"] + args
        self.log.get_buffer().set_text("")
        for w in (self.phase, self.file_label, self.overall_label):
            w.set_text("")
        self.phase.set_text("Starting..."); self.bar.set_fraction(0); self.file_bar.set_fraction(0)
        try:
            self.proc = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        except OSError as e:
            self.phase.set_text(f"Could not start the installer: {e}"); return
        self.install.set_sensitive(False); self.cancel.set_sensitive(True)
        self.started = time.monotonic(); self.result = None
        threading.Thread(target=self.reader, daemon=True).start()

    def reader(self):
        for line in self.proc.stdout:
            GLib.idle_add(self.on_event, line.rstrip("\n"))
        rc = self.proc.wait()
        GLib.idle_add(self.on_exit, rc)

    def on_event(self, line):
        try:
            e = json.loads(line)
        except ValueError:
            self.logline(line); return
        kind = e.get("event"); ph = e.get("phase", "")
        if kind == "phase":
            self.phase.set_text(PHASES.get(ph, ph)); self.logline("== " + PHASES.get(ph, ph))
            if ph in ("copy", "verify"):
                self.bar.set_fraction(0); self.file_bar.set_fraction(0)
            if ph == "flush":
                self.file_label.set_text("Waiting for the drive to finish writing")
        elif kind == "file":
            self.logline(f"{VERB.get(ph, ph)} {e.get('path')} ({human(e.get('size', 0))})")
            self.file_label.set_text(f"{VERB.get(ph, ph)} {e.get('path')}"); self.file_bar.set_fraction(0)
        elif kind == "progress":
            ft, fd = e.get("file_total") or 0, e.get("file_done") or 0
            if ft:
                self.file_bar.set_fraction(min(1.0, fd / ft))
                self.file_label.set_text(f"{VERB.get(ph, ph)} {e.get('path')} ({human(fd)} of {human(ft)})")
            if e.get("total"):
                frac = min(1.0, e["done"] / e["total"])
                self.bar.set_fraction(frac)
                self.overall_label.set_text(f"Overall: {int(frac * 100)}% ({PHASES.get(ph, ph).lower()})")
        elif kind == "log":
            self.logline(e.get("message", ""))
        elif kind == "error":
            self.logline("ERROR: " + e.get("message", ""))
            self.phase.set_markup(f'<span foreground="red">{GLib.markup_escape_text(e.get("message",""))}</span>')
            self.result = ("error", e.get("message", ""))
        elif kind == "canceled":
            self.logline("Canceled."); self.phase.set_text("Canceled. The drive has no partition table; run the installer again to use it.")
            self.result = ("canceled", "")
        elif kind == "done":
            self.bar.set_fraction(1.0); self.file_bar.set_fraction(1.0); self.logline("Done.")
            note = "Verification passed." if e.get("verified") else "Verification skipped."
            mins, secs = divmod(int(time.monotonic() - self.started), 60)
            summary = f"{e.get('message','')}.\n{note} Took {mins} min {secs} s.\nYou can unplug the drive; eject it first if your desktop mounted it."
            self.phase.set_markup(f"<b>Done.</b> {GLib.markup_escape_text(note)} Took {mins} min {secs} s.")
            self.overall_label.set_text("Overall: 100%"); self.file_label.set_text("")
            self.result = ("done", summary)

    def on_exit(self, rc):
        self.proc = None
        self.cancel.set_sensitive(False)
        if rc not in (0, 5, 6) and not self.phase.get_text().startswith("Done"):
            self.phase.set_text(self.phase.get_text() or f"The installer stopped with exit code {rc}. See Details.")
        self.refresh()
        kind, text = self.result or ("error", f"The installer stopped with exit code {rc}. See Details.")
        if kind == "done":
            self.dialog(Gtk.MessageType.INFO, "Parted Magic is on the drive", text)
        elif kind == "canceled":
            self.dialog(Gtk.MessageType.WARNING, "Canceled", "The drive has no partition table. Run the installer again to use it.")
        else:
            self.dialog(Gtk.MessageType.ERROR, "The installer failed", text)

    def on_cancel(self, *_):
        if self.proc and self.proc.stdin:
            try:
                self.proc.stdin.close()
            except OSError:
                pass
            self.phase.set_text("Canceling after the current write...")

if __name__ == "__main__":
    w = App(); w.show_all(); Gtk.main()
