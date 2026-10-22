// Parted Magic USB installer (working name pmusb), macOS window.
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

// The macOS front end, the twin of pmusb-gtk.py. AppKit only, no nib. The
// disk work is done by the pmusb core in Contents/MacOS; this window never
// touches a disk. Everything runs as the user: for the drive itself the core
// asks /usr/libexec/authopen, which shows the system password dialog and
// hands the open disk back. (A root core started through `do shell script
// ... with administrator privileges` was refused the raw disk, 2026-10-21.)
// Closing the core's stdin cancels, as the GTK window closes the pipe.

import AppKit
import UniformTypeIdentifiers

let appTitle = "Parted Magic USB Installer"
let phases = ["source": "Reading the ISO", "prepare": "Preparing the drive", "format": "Formatting",
              "copy": "Writing files", "bootloader": "Installing boot loader", "flush": "Flushing to the drive",
              "verify": "Verifying", "finish": "Finishing"]
let verbs = ["copy": "Copying", "verify": "Verifying"]

func human(_ v: Double) -> String {
    var n = v
    for unit in ["bytes", "KB", "MB", "GB", "TB"] {
        if n < 1000 || unit == "TB" {
            return unit == "bytes" ? "\(Int(n)) bytes" : String(format: "%.1f %@", n, unit)
        }
        n /= 1000
    }
    return ""
}

func num(_ x: Any?) -> Double {
    if let d = x as? Double { return d }
    if let i = x as? Int { return Double(i) }
    if let n = x as? NSNumber { return n.doubleValue }
    return 0
}

// The window's content view takes an ISO dropped from Finder, so the ISO can
// be chosen without the system file panel (it hung on the Air, 2026-10-21).
final class DropView: NSView {
    var onDrop: ((String) -> Void)?

    func isoURL(_ info: NSDraggingInfo) -> URL? {
        let urls = info.draggingPasteboard.readObjects(forClasses: [NSURL.self],
                                                       options: [.urlReadingFileURLsOnly: true]) as? [URL]
        return urls?.first { $0.pathExtension.lowercased() == "iso" }
    }

    override func draggingEntered(_ sender: NSDraggingInfo) -> NSDragOperation {
        return isoURL(sender) == nil ? [] : .copy
    }

    override func performDragOperation(_ sender: NSDraggingInfo) -> Bool {
        guard let u = isoURL(sender) else { return false }
        onDrop?(u.path)
        return true
    }
}

struct Drive {
    let path, model, serial: String
    let size: Double
    let usb, removable: Bool
}

final class AppDelegate: NSObject, NSApplicationDelegate, NSWindowDelegate {
    let defaults = UserDefaults.standard
    var window: NSWindow!
    let core: String? = {
        let p = Bundle.main.bundleURL.appendingPathComponent("Contents/MacOS/pmusb").path
        if FileManager.default.isExecutableFile(atPath: p) { return p }
        let beside = URL(fileURLWithPath: CommandLine.arguments[0]).deletingLastPathComponent().appendingPathComponent("pmusb").path
        return FileManager.default.isExecutableFile(atPath: beside) ? beside : nil
    }()

    // controls
    let isoField = NSTextField(string: "")
    let isoStatus = NSTextField(wrappingLabelWithString: "")
    let driveNote = NSTextField(wrappingLabelWithString: "")
    let drivePopup = NSPopUpButton()
    let labelField = NSTextField(string: "PMAGIC")
    let verifyCB = NSButton(checkboxWithTitle: "Verify after writing (recommended)", target: nil, action: nil)
    let biosCB = NSButton(checkboxWithTitle: "Include legacy BIOS boot loader (syslinux)", target: nil, action: nil)
    let fixedCB = NSButton(checkboxWithTitle: "Show USB hard drives", target: nil, action: nil)
    let allCB = NSButton(checkboxWithTitle: "Show all drives (not only USB)", target: nil, action: nil)
    let advancedBox = NSStackView()
    let advancedToggle = NSButton()
    let installBtn = NSButton(title: "Install", target: nil, action: nil)
    let cancelBtn = NSButton(title: "Cancel", target: nil, action: nil)
    let phaseLabel = NSTextField(wrappingLabelWithString: "")
    let fileLabel = NSTextField(labelWithString: "")
    let fileBar = NSProgressIndicator()
    let overallLabel = NSTextField(labelWithString: "")
    let overallBar = NSProgressIndicator()
    let detailsToggle = NSButton()
    let logScroll = NSScrollView()
    let logView = NSTextView()

    // state
    var noteRow: NSStackView?
    var isoPath: String?
    var source: [String: Any]?
    var drives: [Drive] = []
    var running = false
    var started = Date()
    var result: (String, String)?
    var proc: Process?
    var stdinPipe: Pipe?

    func applicationDidFinishLaunching(_ n: Notification) {
        buildMenu()
        buildWindow()
        if core == nil {
            isoStatus.stringValue = "The pmusb core program was not found inside the application."
        }
        refresh()
        if let last = defaults.string(forKey: "last_iso"), FileManager.default.fileExists(atPath: last) {
            setISO(last)
        }
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ s: NSApplication) -> Bool { true }

    func applicationShouldTerminate(_ s: NSApplication) -> NSApplication.TerminateReply {
        if running {
            let a = NSAlert()
            a.messageText = "The installer is still writing"
            a.informativeText = "Quitting now cancels the install. The drive will have no partition table until you run the installer again."
            a.addButton(withTitle: "Keep Writing")
            a.addButton(withTitle: "Cancel and Quit")
            if a.runModal() == .alertFirstButtonReturn { return .terminateCancel }
            cancelInstall()
        }
        saveSettings()
        return .terminateNow
    }

    func buildMenu() {
        let main = NSMenu()
        let appItem = NSMenuItem()
        main.addItem(appItem)
        let appMenu = NSMenu()
        appMenu.addItem(withTitle: "Quit \(appTitle)", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        appItem.submenu = appMenu
        let editItem = NSMenuItem()
        main.addItem(editItem)
        let edit = NSMenu(title: "Edit")
        edit.addItem(withTitle: "Cut", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        edit.addItem(withTitle: "Copy", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        edit.addItem(withTitle: "Paste", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        edit.addItem(withTitle: "Select All", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        editItem.submenu = edit
        NSApp.mainMenu = main
    }

    func row(_ views: [NSView]) -> NSStackView {
        let s = NSStackView(views: views)
        s.orientation = .horizontal
        s.spacing = 8
        s.alignment = .firstBaseline
        return s
    }

    func buildWindow() {
        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 620, height: 360),
                          styleMask: [.titled, .closable, .miniaturizable, .resizable], backing: .buffered, defer: false)
        window.title = appTitle
        window.delegate = self
        window.center()

        let isoLabel = NSTextField(labelWithString: "Parted Magic ISO")
        let driveLabel = NSTextField(labelWithString: "USB drive")
        for l in [isoLabel, driveLabel] { l.widthAnchor.constraint(equalToConstant: 120).isActive = true }
        isoField.placeholderString = "Choose, drop or paste the ISO"
        isoField.lineBreakMode = .byTruncatingMiddle
        isoField.cell?.usesSingleLineMode = true
        isoField.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
        isoField.target = self
        isoField.action = #selector(isoTyped)
        let browse = NSButton(title: "Choose...", target: self, action: #selector(chooseISO))
        let refreshBtn = NSButton(title: "Refresh", target: self, action: #selector(refreshClicked))
        drivePopup.setContentHuggingPriority(.defaultLow, for: .horizontal)
        // A long item must not widen the window past the screen.
        drivePopup.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
        (drivePopup.cell as? NSPopUpButtonCell)?.lineBreakMode = .byTruncatingTail
        isoStatus.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
        driveNote.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
        drivePopup.target = self
        drivePopup.action = #selector(updateReady)

        let isoRow = row([isoLabel, isoField, browse])
        let statusRow = row([NSTextField(labelWithString: ""), isoStatus])
        statusRow.views[0].widthAnchor.constraint(equalToConstant: 120).isActive = true
        let driveRow = row([driveLabel, drivePopup, refreshBtn])
        let noteRow = row([NSTextField(labelWithString: ""), driveNote])
        noteRow.views[0].widthAnchor.constraint(equalToConstant: 120).isActive = true
        noteRow.isHidden = true
        self.noteRow = noteRow

        // Advanced
        advancedToggle.setButtonType(.pushOnPushOff)
        advancedToggle.bezelStyle = .disclosure
        advancedToggle.title = ""
        advancedToggle.target = self
        advancedToggle.action = #selector(toggleAdvanced)
        let advRow = row([advancedToggle, NSTextField(labelWithString: "Advanced")])
        labelField.stringValue = defaults.string(forKey: "label") ?? "PMAGIC"
        labelField.widthAnchor.constraint(equalToConstant: 110).isActive = true
        verifyCB.state = (defaults.object(forKey: "verify") as? Bool ?? true) ? .on : .off
        biosCB.state = (defaults.object(forKey: "bios") as? Bool ?? true) ? .on : .off
        fixedCB.state = defaults.bool(forKey: "show_fixed") ? .on : .off
        allCB.state = defaults.bool(forKey: "show_all") ? .on : .off
        fixedCB.toolTip = "USB devices that report fixed media, such as SSD enclosures, are hidden by default."
        allCB.toolTip = "Also lists non-USB disks. Disks holding this system, disks with mounted filesystems, and the disk holding the ISO are never offered."
        for cb in [verifyCB, biosCB] { cb.target = self; cb.action = #selector(settingChanged) }
        for cb in [fixedCB, allCB] { cb.target = self; cb.action = #selector(listOptionChanged) }
        advancedBox.orientation = .vertical
        advancedBox.alignment = .leading
        advancedBox.spacing = 4
        advancedBox.addArrangedSubview(row([NSTextField(labelWithString: "Volume label"), labelField]))
        for cb in [verifyCB, biosCB, fixedCB, allCB] { advancedBox.addArrangedSubview(cb) }
        advancedBox.isHidden = true

        // Buttons
        cancelBtn.target = self
        cancelBtn.action = #selector(cancelClicked)
        cancelBtn.isEnabled = false
        installBtn.target = self
        installBtn.action = #selector(installClicked)
        installBtn.keyEquivalent = "\r"
        installBtn.isEnabled = false
        let spacer = NSView()
        spacer.setContentHuggingPriority(.init(1), for: .horizontal)
        let buttons = row([spacer, cancelBtn, installBtn])

        // Progress
        fileLabel.lineBreakMode = .byTruncatingMiddle
        fileLabel.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
        for b in [fileBar, overallBar] {
            b.isIndeterminate = false
            b.minValue = 0
            b.maxValue = 1
            b.style = .bar
        }

        // Details
        detailsToggle.setButtonType(.pushOnPushOff)
        detailsToggle.bezelStyle = .disclosure
        detailsToggle.title = ""
        detailsToggle.target = self
        detailsToggle.action = #selector(toggleDetails)
        let detRow = row([detailsToggle, NSTextField(labelWithString: "Details")])
        logView.isEditable = false
        logView.font = NSFont.monospacedSystemFont(ofSize: 11, weight: .regular)
        logView.isVerticallyResizable = true
        logView.autoresizingMask = [.width]
        logView.textContainer?.widthTracksTextView = true
        logScroll.documentView = logView
        logScroll.hasVerticalScroller = true
        logScroll.heightAnchor.constraint(greaterThanOrEqualToConstant: 160).isActive = true
        logScroll.isHidden = true

        let stack = NSStackView(views: [isoRow, statusRow, driveRow, noteRow, advRow, advancedBox, buttons,
                                        phaseLabel, fileLabel, fileBar, overallLabel, overallBar, detRow, logScroll])
        stack.orientation = .vertical
        stack.alignment = .leading
        stack.spacing = 8
        stack.edgeInsets = NSEdgeInsets(top: 16, left: 16, bottom: 16, right: 16)
        stack.translatesAutoresizingMaskIntoConstraints = false
        for v in [isoRow, statusRow, driveRow, noteRow, buttons, phaseLabel, fileLabel, fileBar, overallLabel, overallBar, logScroll] {
            v.widthAnchor.constraint(equalTo: stack.widthAnchor, constant: -32).isActive = true
        }
        let content = DropView()
        content.registerForDraggedTypes([.fileURL])
        content.onDrop = { [weak self] path in self?.setISO(path) }
        content.addSubview(stack)
        NSLayoutConstraint.activate([
            stack.topAnchor.constraint(equalTo: content.topAnchor),
            stack.leadingAnchor.constraint(equalTo: content.leadingAnchor),
            stack.trailingAnchor.constraint(equalTo: content.trailingAnchor),
            stack.bottomAnchor.constraint(lessThanOrEqualTo: content.bottomAnchor),
            content.widthAnchor.constraint(greaterThanOrEqualToConstant: 560),
        ])
        window.contentView = content
    }

    // MARK: settings

    func saveSettings() {
        defaults.set(verifyCB.state == .on, forKey: "verify")
        defaults.set(biosCB.state == .on, forKey: "bios")
        defaults.set(fixedCB.state == .on, forKey: "show_fixed")
        defaults.set(allCB.state == .on, forKey: "show_all")
        defaults.set(labelField.stringValue, forKey: "label")
    }

    @objc func settingChanged() { saveSettings() }
    @objc func listOptionChanged() { saveSettings(); refresh() }
    @objc func refreshClicked() { refresh() }
    @objc func toggleAdvanced() { advancedBox.isHidden = advancedToggle.state != .on }
    @objc func toggleDetails() { logScroll.isHidden = detailsToggle.state != .on }

    // MARK: core calls as the user

    func runJSON(_ args: [String]) -> ([[String: Any]], Int32) {
        guard let core = core else { return ([], 1) }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: core)
        p.arguments = args + ["--json"]
        let out = Pipe()
        p.standardOutput = out
        p.standardError = out
        do { try p.run() } catch { return ([], 1) }
        let data = out.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        var events: [[String: Any]] = []
        for line in String(decoding: data, as: UTF8.self).split(separator: "\n") {
            let t = line.trimmingCharacters(in: .whitespaces)
            if t.hasPrefix("{"), let o = try? JSONSerialization.jsonObject(with: Data(t.utf8)) as? [String: Any] {
                events.append(o)
            }
        }
        return (events, p.terminationStatus)
    }

    func refresh() {
        var args = ["list-devices"]
        if allCB.state == .on { args.append("--all") }
        if fixedCB.state == .on { args.append("--fixed") }
        let (events, _) = runJSON(args)
        drives = []
        let notReadyEvents = events.filter { $0["event"] as? String == "nomedium" }
        let notReady = notReadyEvents.compactMap { $0["name"] as? String }
        for e in events where e["event"] as? String == "devices" {
            for d in (e["devices"] as? [[String: Any]]) ?? [] {
                drives.append(Drive(path: d["Path"] as? String ?? "", model: d["Model"] as? String ?? "",
                                    serial: d["Serial"] as? String ?? "", size: num(d["Size"]),
                                    usb: d["USB"] as? Bool ?? false, removable: d["Removable"] as? Bool ?? false))
            }
        }
        drivePopup.removeAllItems()
        for d in drives {
            drivePopup.addItem(withTitle: "\(d.model), \(human(d.size)), \(d.usb ? "USB" : "not USB"), \(d.path)")
        }
        if drives.isEmpty && notReady.isEmpty {
            drivePopup.addItem(withTitle: "No USB drive found. Plug one in and click Refresh.")
        }
        // Drives with no medium are listed so their owner knows why, but
        // they cannot be chosen.
        drivePopup.autoenablesItems = false
        for m in notReady {
            drivePopup.addItem(withTitle: "\(m): not ready")
            drivePopup.lastItem?.isEnabled = false
        }
        // The advice goes under the list, where it can wrap.
        driveNote.stringValue = notReadyEvents.compactMap { $0["message"] as? String }.joined(separator: "\n")
        noteRow?.isHidden = notReady.isEmpty
        drivePopup.selectItem(at: 0)
        updateReady()
    }

    @objc func chooseISO() {
        let panel = NSOpenPanel()
        panel.title = "Choose the Parted Magic ISO"
        panel.allowsMultipleSelection = false
        panel.canChooseDirectories = false
        if let iso = UTType(filenameExtension: "iso") { panel.allowedContentTypes = [iso] }
        if let dir = defaults.string(forKey: "last_iso_dir") { panel.directoryURL = URL(fileURLWithPath: dir) }
        panel.beginSheetModal(for: window) { r in
            if r == .OK, let u = panel.url { self.setISO(u.path) }
        }
    }

    @objc func isoTyped() {
        let t = (isoField.stringValue.trimmingCharacters(in: .whitespacesAndNewlines) as NSString).expandingTildeInPath
        if t.isEmpty { return }
        if t != isoPath { setISO(t) }
    }

    func setISO(_ path: String) {
        guard !running else { return }
        isoPath = path
        source = nil
        isoField.stringValue = path
        defaults.set(path, forKey: "last_iso")
        defaults.set((path as NSString).deletingLastPathComponent, forKey: "last_iso_dir")
        guard let core = core else { updateReady(); return }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: core)
        p.arguments = ["inspect", "--source", path, "--json"]
        let out = Pipe(), err = Pipe()
        p.standardOutput = out
        p.standardError = err
        try? p.run()
        let data = out.fileHandleForReading.readDataToEndOfFile()
        let edata = err.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        let info = try? JSONSerialization.jsonObject(with: data) as? [String: Any]
        if p.terminationStatus != 0 || info == nil {
            var msg = "Not a Parted Magic ISO"
            for line in (String(decoding: data, as: UTF8.self) + "\n" + String(decoding: edata, as: UTF8.self)).split(separator: "\n")
            where line.hasPrefix("{") {
                if let o = try? JSONSerialization.jsonObject(with: Data(line.utf8)) as? [String: Any], let m = o["message"] as? String {
                    msg = m
                }
            }
            isoStatus.stringValue = msg
            isoStatus.textColor = .systemRed
        } else {
            source = info
            isoStatus.textColor = .labelColor
            let build = (info?["Build"] as? String).map { ", build \($0)" } ?? ""
            var txt = "Parted Magic \(info?["Version"] as? String ?? "")\(build), \(human(num(info?["Bytes"])))"
            if !(info?["BIOSModulesMatch"] as? Bool ?? false) {
                txt += " (BIOS modules newer than this installer: UEFI-only)"
                biosCB.state = .off
            }
            isoStatus.stringValue = txt
        }
        updateReady()
    }

    func selectedDrive() -> Drive? {
        let i = drivePopup.indexOfSelectedItem
        return i >= 0 && i < drives.count ? drives[i] : nil
    }

    @objc func updateReady() {
        let d = selectedDrive()
        var ok = source != nil && d != nil && !running
        installBtn.toolTip = nil
        if ok, let d = d, let s = source, d.size < num(s["RequiredBytes"]) {
            ok = false
            installBtn.toolTip = "Drive too small: needs \(human(num(s["RequiredBytes"])))"
        }
        installBtn.isEnabled = ok
    }

    // MARK: install as root

    func logLine(_ s: String) {
        let at = NSAttributedString(string: s + "\n", attributes: [.font: logView.font as Any, .foregroundColor: NSColor.labelColor])
        logView.textStorage?.append(at)
        logView.scrollToEndOfDocument(nil)
    }

    @objc func installClicked() {
        guard let d = selectedDrive(), let info = source, let iso = isoPath, let core = core else { return }
        let build = (info["Build"] as? String).map { " (build \($0))" } ?? ""
        var text = "\(d.model), \(human(d.size)), \(d.usb ? "USB" : "not USB"), serial \(d.serial.isEmpty ? "unknown" : d.serial)\n"
            + "Device \(d.path)\n\nParted Magic \(info["Version"] as? String ?? "")\(build) will be installed from:\n\(iso)\n\nThis cannot be undone."
        if d.usb && !d.removable {
            text += "\n\nThis device reports itself as a hard drive. Parted Magic will take about 10 seconds longer to find it at boot."
        }
        let a = NSAlert()
        a.alertStyle = .warning
        a.messageText = "Everything on this drive will be erased."
        a.informativeText = text
        a.addButton(withTitle: "Cancel")
        a.addButton(withTitle: "Erase and Install")
        guard a.runModal() == .alertSecondButtonReturn else { return }
        saveSettings()

        var args = ["install", "--source", iso, "--target", d.path, "--confirm-erase",
                    "--expect", "\(d.model)|\(d.serial)|\(Int64(d.size))",
                    "--label", labelField.stringValue.isEmpty ? "PMAGIC" : labelField.stringValue,
                    "--json", "--cancel-on-eof"]
        if verifyCB.state != .on { args.append("--no-verify") }
        if biosCB.state != .on { args.append("--no-bios") }
        if allCB.state == .on { args.append("--allow-non-usb") }
        if fixedCB.state == .on { args.append("--allow-fixed") }

        logView.string = ""
        fileLabel.stringValue = ""
        overallLabel.stringValue = ""
        phaseLabel.textColor = .labelColor
        phaseLabel.stringValue = "Starting..."
        fileBar.doubleValue = 0
        overallBar.doubleValue = 0
        result = nil

        let p = Process()
        p.executableURL = URL(fileURLWithPath: core)
        p.arguments = args
        let inP = Pipe(), outP = Pipe()
        p.standardInput = inP
        p.standardOutput = outP
        p.standardError = outP
        p.terminationHandler = { proc in
            let code = proc.terminationStatus
            // Let the reader drain the last lines before the result is shown.
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.3) { self.finish(code) }
        }
        do { try p.run() } catch {
            phaseLabel.stringValue = "Could not start the installer: \(error.localizedDescription)"
            return
        }
        proc = p
        stdinPipe = inP
        running = true
        started = Date()
        installBtn.isEnabled = false
        cancelBtn.isEnabled = true

        Thread.detachNewThread {
            let h = outP.fileHandleForReading
            var buf = Data()
            while true {
                let chunk = h.availableData
                if chunk.isEmpty { break }
                buf.append(chunk)
                while let nl = buf.firstIndex(of: 0x0A) {
                    let line = String(decoding: buf[buf.startIndex..<nl], as: UTF8.self)
                    buf.removeSubrange(buf.startIndex...nl)
                    DispatchQueue.main.async { self.onEvent(line) }
                }
            }
        }
    }

    func onEvent(_ line: String) {
        guard let e = try? JSONSerialization.jsonObject(with: Data(line.utf8)) as? [String: Any] else {
            if !line.isEmpty { logLine(line) }
            return
        }
        let kind = e["event"] as? String ?? ""
        let ph = e["phase"] as? String ?? ""
        let phName = phases[ph] ?? ph
        let verb = verbs[ph] ?? ph
        switch kind {
        case "phase":
            phaseLabel.stringValue = phName
            logLine("== " + phName)
            if ph == "copy" || ph == "verify" { overallBar.doubleValue = overallBar.doubleValue; fileBar.doubleValue = 0 }
            if ph == "flush" { fileLabel.stringValue = "Waiting for the drive to finish writing" }
        case "file":
            logLine("\(verb) \(e["path"] as? String ?? "") (\(human(num(e["size"]))))")
            fileLabel.stringValue = "\(verb) \(e["path"] as? String ?? "")"
            fileBar.doubleValue = 0
        case "progress":
            let ft = num(e["file_total"]), fd = num(e["file_done"])
            if ft > 0 {
                fileBar.doubleValue = min(1, fd / ft)
                fileLabel.stringValue = "\(verb) \(e["path"] as? String ?? "") (\(human(fd)) of \(human(ft)))"
            }
            let total = num(e["total"])
            if total > 0 {
                let frac = min(1, num(e["done"]) / total)
                overallBar.doubleValue = frac
                overallLabel.stringValue = "Overall: \(Int(frac * 100))% (\(phName.lowercased()))"
            }
        case "log":
            logLine(e["message"] as? String ?? "")
        case "error":
            let m = e["message"] as? String ?? ""
            logLine("ERROR: " + m)
            phaseLabel.stringValue = m
            phaseLabel.textColor = .systemRed
            result = ("error", m)
        case "canceled":
            logLine("Canceled.")
            phaseLabel.stringValue = "Canceled. The drive has no partition table; run the installer again to use it."
            result = ("canceled", "")
        case "done":
            fileBar.doubleValue = 1
            overallBar.doubleValue = 1
            logLine("Done.")
            let note = (e["verified"] as? Bool ?? false) ? "Verification passed." : "Verification skipped."
            let secs = Int(Date().timeIntervalSince(started))
            phaseLabel.stringValue = "Done. \(note) Took \(secs / 60) min \(secs % 60) s."
            overallLabel.stringValue = "Overall: 100%"
            fileLabel.stringValue = ""
            result = ("done", "\(e["message"] as? String ?? "").\n\(note) Took \(secs / 60) min \(secs % 60) s.\nThe drive has been ejected. You can unplug it.")
        default:
            break
        }
    }

    func finish(_ code: Int32) {
        guard running else { return }
        running = false
        cancelBtn.isEnabled = false
        try? stdinPipe?.fileHandleForWriting.close()
        stdinPipe = nil
        proc = nil
        refresh()
        let a = NSAlert()
        switch result?.0 ?? "error" {
        case "done":
            a.messageText = "Parted Magic is on the drive"
            a.informativeText = result?.1 ?? ""
        case "canceled":
            a.alertStyle = .warning
            a.messageText = "Canceled"
            a.informativeText = "The drive has no partition table. Run the installer again to use it."
        default:
            a.alertStyle = .critical
            a.messageText = "The installer failed"
            let m = result?.1 ?? ""
            a.informativeText = m.isEmpty ? "The installer stopped with exit code \(code). See Details." : m
            if phaseLabel.stringValue.isEmpty || phaseLabel.stringValue == "Starting..." {
                phaseLabel.stringValue = a.informativeText
            }
        }
        a.beginSheetModal(for: window, completionHandler: nil)
    }

    func cancelInstall() {
        if let p = stdinPipe {
            try? p.fileHandleForWriting.close()
            stdinPipe = nil
            phaseLabel.stringValue = "Canceling after the current write..."
        }
    }

    @objc func cancelClicked() { cancelInstall() }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.regular)
app.run()
