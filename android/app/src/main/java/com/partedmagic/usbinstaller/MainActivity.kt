// Copyright 2026 Patrick J. Verner
// SPDX-License-Identifier: GPL-3.0-or-later

package com.partedmagic.usbinstaller

import android.Manifest
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.hardware.usb.UsbDevice
import android.hardware.usb.UsbManager
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.provider.DocumentsContract
import android.provider.OpenableColumns
import androidx.activity.ComponentActivity
import androidx.activity.compose.BackHandler
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.systemBarsPadding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableFloatStateOf
import androidx.compose.runtime.mutableStateListOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalView
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.core.content.ContextCompat
import androidx.core.content.IntentCompat
import com.partedmagic.usbinstaller.ui.Accent
import com.partedmagic.usbinstaller.ui.BadRed
import com.partedmagic.usbinstaller.ui.BgCard
import com.partedmagic.usbinstaller.ui.PMTheme
import com.partedmagic.usbinstaller.ui.TxtDim
import org.json.JSONObject
import java.util.concurrent.Executors

private val PHASES = mapOf(
    "source" to "Reading the ISO", "prepare" to "Preparing the drive", "format" to "Formatting",
    "copy" to "Writing files", "bootloader" to "Installing boot loader", "flush" to "Flushing to the drive",
    "verify" to "Verifying", "finish" to "Finishing",
)
private val VERBS = mapOf("copy" to "Copying", "verify" to "Verifying")

fun human(v: Double): String {
    var n = v
    for (unit in listOf("bytes", "KB", "MB", "GB", "TB")) {
        if (n < 1000 || unit == "TB") {
            return if (unit == "bytes") "${n.toLong()} bytes" else String.format(java.util.Locale.US, "%.1f %s", n, unit)
        }
        n /= 1000.0
    }
    return ""
}

/** Everything the window shows. It lives outside the activity, so an
 *  install keeps going, and keeps its progress, if the window is rebuilt. */
object State {
    var isoUri by mutableStateOf<Uri?>(null)
    var isoName by mutableStateOf("")
    var iso by mutableStateOf<JSONObject?>(null)
    var isoStatus by mutableStateOf("")
    var isoBad by mutableStateOf(false)

    val drives = mutableStateListOf<UsbDevice>()
    var selected by mutableStateOf<String?>(null) // deviceName
    var open: UsbDrive.Open? = null
    var drive by mutableStateOf<JSONObject?>(null) // the core's list-devices entry
    var driveStatus by mutableStateOf("")
    var driveBad by mutableStateOf(false)

    var label by mutableStateOf("PMAGIC")
    var verify by mutableStateOf(true)
    var bios by mutableStateOf(true)
    var advanced by mutableStateOf(false)
    var details by mutableStateOf(false)

    var running by mutableStateOf(false)
    var run: CoreRun? = null
    var phase by mutableStateOf("")
    var phaseBad by mutableStateOf(false)
    var fileLine by mutableStateOf("")
    var fileFrac by mutableFloatStateOf(0f)
    var overallLine by mutableStateOf("")
    var overallFrac by mutableFloatStateOf(0f)
    val log = mutableStateListOf<String>()
    var confirm by mutableStateOf<String?>(null)
    var result by mutableStateOf<Pair<String, String>?>(null)
    var resultKind = ""
    var resultText = ""
    var started = 0L
}

class MainActivity : ComponentActivity() {
    private lateinit var usb: UsbManager
    private val io = Executors.newSingleThreadExecutor()

    private fun prefs() = getSharedPreferences("pmusb", MODE_PRIVATE)

    private val askNotifications =
        registerForActivityResult(ActivityResultContracts.RequestPermission()) {}

    private val pickIso = registerForActivityResult(ActivityResultContracts.OpenDocument()) { uri ->
        if (uri != null) {
            try {
                contentResolver.takePersistableUriPermission(uri, Intent.FLAG_GRANT_READ_URI_PERMISSION)
            } catch (_: Exception) {
            }
            prefs().edit().putString("last_iso", uri.toString()).apply()
            setIso(uri)
        }
    }

    private val usbEvents = object : BroadcastReceiver() {
        override fun onReceive(c: Context, i: Intent) {
            val dev = IntentCompat.getParcelableExtra(i, UsbManager.EXTRA_DEVICE, UsbDevice::class.java)
            when (i.action) {
                UsbDrive.ACTION_PERMISSION ->
                    if (dev != null && i.getBooleanExtra(UsbManager.EXTRA_PERMISSION_GRANTED, false)) {
                        openDrive(dev)
                    } else {
                        State.driveStatus = "Access to the drive was not allowed."
                        State.driveBad = true
                    }
                UsbManager.ACTION_USB_DEVICE_DETACHED -> {
                    if (dev != null && dev.deviceName == State.open?.device?.deviceName && !State.running) dropDrive()
                    refresh()
                }
                UsbManager.ACTION_USB_DEVICE_ATTACHED -> refresh()
            }
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        usb = getSystemService(UsbManager::class.java)
        val p = prefs()
        if (!State.running) {
            State.label = p.getString("label", "PMAGIC") ?: "PMAGIC"
            State.verify = p.getBoolean("verify", true)
            State.bios = p.getBoolean("bios", true)
        }
        val f = IntentFilter().apply {
            addAction(UsbDrive.ACTION_PERMISSION)
            addAction(UsbManager.ACTION_USB_DEVICE_ATTACHED)
            addAction(UsbManager.ACTION_USB_DEVICE_DETACHED)
        }
        ContextCompat.registerReceiver(this, usbEvents, f, ContextCompat.RECEIVER_NOT_EXPORTED)
        refresh()
        if (State.isoUri == null) {
            p.getString("last_iso", null)?.let { s ->
                val uri = Uri.parse(s)
                if (contentResolver.persistedUriPermissions.any { it.uri == uri && it.isReadPermission }) setIso(uri)
            }
        }
        setContent { PMTheme { Screen() } }
    }

    override fun onDestroy() {
        unregisterReceiver(usbEvents)
        super.onDestroy()
    }

    private fun saveSettings() {
        prefs().edit().putString("label", State.label).putBoolean("verify", State.verify)
            .putBoolean("bios", State.bios).apply()
    }

    // ---- the ISO ----

    private fun displayName(uri: Uri): String {
        try {
            contentResolver.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)?.use { c ->
                if (c.moveToFirst()) return c.getString(0) ?: ""
            }
        } catch (_: Exception) {
        }
        return uri.lastPathSegment ?: ""
    }

    private fun setIso(uri: Uri) {
        if (State.running) return
        State.isoUri = uri
        State.isoName = displayName(uri)
        State.iso = null
        State.isoBad = false
        // An ISO on a USB drive or SD card could be on the very stick about
        // to be erased, and a claimed stick vanishes from the phone's files.
        if (uri.authority == "com.android.externalstorage.documents" &&
            !DocumentsContract.getDocumentId(uri).startsWith("primary:")
        ) {
            State.isoStatus = "This ISO is on a removable drive. Copy it to the phone first, for example to Downloads."
            State.isoBad = true
            return
        }
        State.isoStatus = "Reading the ISO..."
        io.execute {
            val lines = mutableListOf<String>()
            var rc = 1
            try {
                contentResolver.openFileDescriptor(uri, "r")?.use { pfd ->
                    rc = CoreRun(this, listOf("inspect", "--source", "fd:iso", "--json"), Handoff(iso = pfd))
                        .run { lines += it }
                }
            } catch (e: Exception) {
                lines += JSONObject().put("event", "error").put("message", e.message ?: "cannot open the file").toString()
            }
            val text = lines.joinToString("\n")
            runOnUiThread {
                val info = if (rc == 0) try {
                    JSONObject(text)
                } catch (_: Exception) {
                    null
                } else null
                if (info == null) {
                    var msg = "Not a Parted Magic ISO"
                    for (l in lines) if (l.startsWith("{")) try {
                        JSONObject(l).optString("message").takeIf { it.isNotEmpty() }?.let { msg = it }
                    } catch (_: Exception) {
                    }
                    State.isoStatus = msg
                    State.isoBad = true
                } else {
                    State.iso = info
                    val build = info.optString("Build").takeIf { it.isNotEmpty() }?.let { ", build $it" } ?: ""
                    var txt = "Parted Magic ${info.optString("Version")}$build, ${human(info.optDouble("Bytes"))}"
                    if (!info.optBoolean("BIOSModulesMatch")) {
                        txt += " (BIOS modules newer than this installer: UEFI-only)"
                        State.bios = false
                    }
                    State.isoStatus = txt
                }
            }
        }
    }

    // ---- the drive ----

    private fun refresh() {
        if (State.running) return
        State.drives.clear()
        State.drives.addAll(UsbDrive.list(usb))
        if (State.drives.none { it.deviceName == State.selected }) {
            State.selected = null
            dropDrive()
            State.driveStatus = ""
            State.driveBad = false
        }
    }

    private fun dropDrive() {
        State.open?.close()
        State.open = null
        State.drive = null
    }

    private fun selectDrive(dev: UsbDevice) {
        if (State.running) return
        dropDrive()
        State.selected = dev.deviceName
        State.driveBad = false
        if (usb.hasPermission(dev)) {
            openDrive(dev)
        } else {
            State.driveStatus = "Allow the installer to use this drive."
            val pi = PendingIntent.getBroadcast(
                this, 0, Intent(UsbDrive.ACTION_PERMISSION).setPackage(packageName),
                PendingIntent.FLAG_MUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
            )
            usb.requestPermission(dev, pi)
        }
    }

    private fun openDrive(dev: UsbDevice) {
        if (dev.deviceName != State.selected) return
        State.driveStatus = "Reading the drive..."
        State.driveBad = false
        io.execute {
            val o = UsbDrive.open(usb, dev)
            if (o == null) {
                runOnUiThread {
                    State.driveStatus = "The drive could not be opened. Unplug it, plug it in again and tap it."
                    State.driveBad = true
                }
                return@execute
            }
            val lines = mutableListOf<String>()
            CoreRun(this, listOf("list-devices", "--fixed", "--json"), Handoff(usb = o)).run { lines += it }
            var found: JSONObject? = null
            var msg = "The drive did not answer. Unplug it, wait a few seconds, plug it in again and tap it."
            for (l in lines) if (l.startsWith("{")) try {
                val e = JSONObject(l)
                when (e.optString("event")) {
                    "devices" -> e.optJSONArray("devices")?.optJSONObject(0)?.let { found = it }
                    "error" -> msg = e.optString("message", msg)
                }
            } catch (_: Exception) {
            }
            runOnUiThread {
                val d = found
                if (d == null) {
                    o.close()
                    State.driveStatus = msg
                    State.driveBad = true
                } else {
                    State.open = o
                    State.drive = d
                    State.driveStatus = "${d.optString("Model")}, ${human(d.optDouble("Size"))}, USB"
                }
            }
        }
    }

    // ---- installing ----

    private fun ready(): Boolean {
        val d = State.drive ?: return false
        val i = State.iso ?: return false
        return !State.running && State.open != null && d.optDouble("Size") >= i.optDouble("RequiredBytes")
    }

    private fun askInstall() {
        val d = State.drive ?: return
        val i = State.iso ?: return
        val build = i.optString("Build").takeIf { it.isNotEmpty() }?.let { " (build $it)" } ?: ""
        var text = "Everything on this drive will be erased.\n\n" +
            "${d.optString("Model")}, ${human(d.optDouble("Size"))}, USB, serial ${d.optString("Serial").ifEmpty { "unknown" }}\n\n" +
            "Parted Magic ${i.optString("Version")}$build will be installed from:\n${State.isoName}\n\nThis cannot be undone."
        if (!d.optBoolean("Removable", true)) {
            text += "\n\nThis device reports itself as a hard drive. Parted Magic will take about 10 seconds longer to find it at boot."
        }
        State.confirm = text
    }

    private fun startInstall() {
        val o = State.open ?: return
        val d = State.drive ?: return
        val uri = State.isoUri ?: return
        saveSettings()
        val args = mutableListOf(
            "install", "--source", "fd:iso", "--target", "usb:", "--confirm-erase",
            "--expect", "${d.optString("Model")}|${d.optString("Serial")}|${d.optLong("Size")}",
            "--label", State.label.ifBlank { "PMAGIC" }, "--json", "--cancel-on-eof",
        )
        if (!State.verify) args += "--no-verify"
        if (!State.bios) args += "--no-bios"
        if (!d.optBoolean("Removable", true)) args += "--allow-fixed"
        State.log.clear()
        State.phase = "Starting..."
        State.phaseBad = false
        State.fileLine = ""
        State.overallLine = ""
        State.fileFrac = 0f
        State.overallFrac = 0f
        State.result = null
        State.resultKind = ""
        State.resultText = ""
        State.running = true
        State.started = System.currentTimeMillis()
        // The notification shows the progress while the screen is off.
        if (Build.VERSION.SDK_INT >= 33) askNotifications.launch(Manifest.permission.POST_NOTIFICATIONS)
        InstallService.start(this)
        val saved = java.io.File(filesDir, "last-install.log")
        io.execute {
            var rc = 1
            try {
                contentResolver.openFileDescriptor(uri, "r")?.use { pfd ->
                    val r = CoreRun(this, args, Handoff(o, pfd), saved)
                    State.run = r
                    rc = r.run { line -> runOnUiThread { onEvent(line) } }
                }
            } catch (e: Exception) {
                runOnUiThread {
                    State.resultKind = "error"
                    State.resultText = "Could not start the installer: ${e.message}"
                }
            }
            runOnUiThread { finishInstall(rc) }
        }
    }

    private fun logLine(s: String) {
        State.log += s
    }

    private fun onEvent(line: String) {
        val e = try {
            JSONObject(line)
        } catch (_: Exception) {
            if (line.isNotBlank()) logLine(line)
            return
        }
        val ph = e.optString("phase")
        val phName = PHASES[ph] ?: ph
        val verb = VERBS[ph] ?: ph
        when (e.optString("event")) {
            "phase" -> {
                State.phase = phName
                logLine("== $phName")
                if (ph == "copy" || ph == "verify") State.fileFrac = 0f
                if (ph == "flush") State.fileLine = "Waiting for the drive to finish writing"
            }
            "file" -> {
                logLine("$verb ${e.optString("path")} (${human(e.optDouble("size"))})")
                State.fileLine = "$verb ${e.optString("path")}"
                State.fileFrac = 0f
            }
            "progress" -> {
                val ft = e.optDouble("file_total")
                val fd = e.optDouble("file_done")
                if (ft > 0) {
                    State.fileFrac = (fd / ft).coerceAtMost(1.0).toFloat()
                    State.fileLine = "$verb ${e.optString("path")} (${human(fd)} of ${human(ft)})"
                }
                val total = e.optDouble("total")
                if (total > 0) {
                    val frac = (e.optDouble("done") / total).coerceAtMost(1.0)
                    State.overallFrac = frac.toFloat()
                    State.overallLine = "Overall: ${(frac * 100).toInt()}% (${phName.lowercase()})"
                    InstallService.progress(this, State.overallLine, (frac * 100).toInt())
                }
            }
            "log" -> logLine(e.optString("message"))
            "error" -> {
                val m = e.optString("message")
                logLine("ERROR: $m")
                State.phase = m
                State.phaseBad = true
                State.resultKind = "error"
                State.resultText = m
            }
            "canceled" -> {
                logLine("Canceled.")
                State.phase = "Canceled. The drive has no partition table; run the installer again to use it."
                State.resultKind = "canceled"
            }
            "done" -> {
                State.fileFrac = 1f
                State.overallFrac = 1f
                logLine("Done.")
                val note = if (e.optBoolean("verified")) "Verification passed." else "Verification skipped."
                val secs = ((System.currentTimeMillis() - State.started) / 1000).toInt()
                State.phase = "Done. $note Took ${secs / 60} min ${secs % 60} s."
                State.overallLine = "Overall: 100%"
                State.fileLine = ""
                State.resultKind = "done"
                State.resultText = "${e.optString("message")}.\n$note Took ${secs / 60} min ${secs % 60} s.\n" +
                    "The drive has been ejected. You can unplug it."
            }
        }
    }

    private fun finishInstall(rc: Int) {
        State.running = false
        State.run = null
        // Done, the stick is ejected; failed, its state is unknown. Either
        // way it is read again before the next install.
        dropDrive()
        State.selected = null
        State.driveStatus = ""
        refresh()
        val r = when (State.resultKind) {
            "done" -> "Parted Magic is on the drive" to State.resultText
            "canceled" -> "Canceled" to "The drive has no partition table. Run the installer again to use it."
            else -> "The installer failed" to State.resultText.ifEmpty { "The installer stopped with exit code $rc. See Details." }
        }
        State.result = r
        InstallService.stop(this, r.first, r.second.lineSequence().first())
    }

    private fun cancelInstall() {
        State.run?.let {
            it.cancel()
            State.phase = "Canceling after the current write..."
        }
    }

    // ---- the window ----

    @Composable
    private fun Screen() {
        val view = LocalView.current
        DisposableEffect(State.running) {
            view.keepScreenOn = State.running
            onDispose { }
        }
        BackHandler(enabled = State.running) { }
        Surface(Modifier.fillMaxSize()) {
            Column(
                Modifier.fillMaxSize().systemBarsPadding().verticalScroll(rememberScrollState()).padding(16.dp),
                verticalArrangement = Arrangement.spacedBy(10.dp),
            ) {
                Text("Parted Magic USB Installer", fontSize = 20.sp, fontWeight = FontWeight.SemiBold)

                Text("Parted Magic ISO", fontWeight = FontWeight.SemiBold)
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Text(
                        State.isoName.ifEmpty { "No ISO chosen" }, Modifier.weight(1f),
                        maxLines = 1, overflow = TextOverflow.Ellipsis, color = if (State.isoName.isEmpty()) TxtDim else androidx.compose.ui.graphics.Color.Unspecified,
                    )
                    OutlinedButton(onClick = { pickIso.launch(arrayOf("*/*")) }, enabled = !State.running) { Text("Choose...") }
                }
                if (State.isoStatus.isNotEmpty()) {
                    Text(State.isoStatus, color = if (State.isoBad) BadRed else TxtDim, fontSize = 14.sp)
                }

                Row(verticalAlignment = Alignment.CenterVertically) {
                    Text("USB drive", fontWeight = FontWeight.SemiBold, modifier = Modifier.weight(1f))
                    OutlinedButton(onClick = { refresh() }, enabled = !State.running) { Text("Refresh") }
                }
                if (State.drives.isEmpty()) {
                    Text("No USB drive found. Plug one in and tap Refresh.", color = TxtDim, fontSize = 14.sp)
                }
                for (d in State.drives) {
                    val sel = d.deviceName == State.selected
                    Card(
                        Modifier.fillMaxWidth().clickable(enabled = !State.running) { selectDrive(d) },
                        colors = CardDefaults.cardColors(containerColor = BgCard),
                    ) {
                        Row(Modifier.padding(10.dp), verticalAlignment = Alignment.CenterVertically) {
                            RadioButton(selected = sel, onClick = { selectDrive(d) }, enabled = !State.running)
                            Column {
                                Text(UsbDrive.label(d))
                                if (sel && State.driveStatus.isNotEmpty()) {
                                    Text(State.driveStatus, color = if (State.driveBad) BadRed else TxtDim, fontSize = 14.sp)
                                } else if (!sel) {
                                    Text("Tap to use this drive", color = TxtDim, fontSize = 14.sp)
                                }
                            }
                        }
                    }
                }

                Text(
                    (if (State.advanced) "▾ " else "▸ ") + "Advanced",
                    Modifier.clickable { State.advanced = !State.advanced }.padding(vertical = 4.dp),
                    color = Accent,
                )
                if (State.advanced) {
                    OutlinedTextField(
                        value = State.label,
                        onValueChange = { State.label = it.take(11) },
                        label = { Text("Volume label") },
                        singleLine = true,
                        enabled = !State.running,
                    )
                    SwitchRow("Verify after writing (recommended)", State.verify) { State.verify = it }
                    SwitchRow("Include legacy BIOS boot loader (syslinux)", State.bios) { State.bios = it }
                }

                Row(horizontalArrangement = Arrangement.spacedBy(8.dp), modifier = Modifier.fillMaxWidth()) {
                    Spacer(Modifier.weight(1f))
                    OutlinedButton(onClick = { cancelInstall() }, enabled = State.running) { Text("Cancel") }
                    Button(onClick = { askInstall() }, enabled = ready()) { Text("Install") }
                }
                if (!ready() && !State.running) {
                    val d = State.drive
                    val i = State.iso
                    if (d != null && i != null && d.optDouble("Size") < i.optDouble("RequiredBytes")) {
                        Text("Drive too small: needs ${human(i.optDouble("RequiredBytes"))}", color = BadRed, fontSize = 14.sp)
                    }
                }

                if (State.phase.isNotEmpty()) {
                    Text(State.phase, color = if (State.phaseBad) BadRed else androidx.compose.ui.graphics.Color.Unspecified)
                }
                if (State.running || State.fileLine.isNotEmpty()) {
                    Text(State.fileLine, maxLines = 1, overflow = TextOverflow.Ellipsis, fontSize = 14.sp, color = TxtDim)
                    LinearProgressIndicator(progress = { State.fileFrac }, modifier = Modifier.fillMaxWidth())
                }
                if (State.running || State.overallLine.isNotEmpty()) {
                    Text(State.overallLine, fontSize = 14.sp, color = TxtDim)
                    LinearProgressIndicator(progress = { State.overallFrac }, modifier = Modifier.fillMaxWidth())
                }

                Text(
                    (if (State.details) "▾ " else "▸ ") + "Details",
                    Modifier.clickable { State.details = !State.details }.padding(vertical = 4.dp),
                    color = Accent,
                )
                if (State.details) {
                    Card(colors = CardDefaults.cardColors(containerColor = BgCard), modifier = Modifier.fillMaxWidth()) {
                        Text(
                            State.log.takeLast(400).joinToString("\n"),
                            Modifier.padding(8.dp),
                            fontFamily = FontFamily.Monospace, fontSize = 11.sp,
                        )
                    }
                }
                Spacer(Modifier.height(24.dp))
            }
        }

        State.confirm?.let { text ->
            AlertDialog(
                onDismissRequest = { State.confirm = null },
                title = { Text("Erase this drive?") },
                text = { Text(text) },
                confirmButton = {
                    TextButton(onClick = {
                        State.confirm = null
                        startInstall()
                    }) { Text("Erase and Install", color = BadRed) }
                },
                dismissButton = { TextButton(onClick = { State.confirm = null }) { Text("Cancel") } },
            )
        }
        State.result?.let { (title, text) ->
            AlertDialog(
                onDismissRequest = { State.result = null },
                title = { Text(title) },
                text = { Text(text) },
                confirmButton = { TextButton(onClick = { State.result = null }) { Text("OK") } },
            )
        }
    }

    @Composable
    private fun SwitchRow(text: String, value: Boolean, set: (Boolean) -> Unit) {
        Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.fillMaxWidth()) {
            Text(text, Modifier.weight(1f))
            Switch(checked = value, onCheckedChange = { set(it); saveSettings() }, enabled = !State.running)
        }
    }
}
