// Copyright 2026 Patrick J. Verner
// SPDX-License-Identifier: GPL-3.0-or-later

package com.partedmagic.usbinstaller

import android.content.Context
import android.net.LocalServerSocket
import android.net.LocalSocket
import android.net.LocalSocketAddress
import android.os.ParcelFileDescriptor
import android.util.Log
import java.io.File
import java.io.FileDescriptor
import java.util.UUID
import kotlin.concurrent.thread

/** What the core is handed: the claimed USB drive and/or the ISO. */
class Handoff(val usb: UsbDrive.Open? = null, val iso: ParcelFileDescriptor? = null)

/**
 * One run of the pmusb core, the same program as on Linux, Windows and the
 * Mac, built for Android and shipped as lib/arm64-v8a/libpmusb.so so it
 * lands in nativeLibraryDir, the one place an app may run a program from.
 *
 * The core cannot open the drive or the ISO itself. When it needs them it
 * connects to the abstract socket named in PMUSB_FD_SOCKET, and this side
 * answers with one line naming the descriptors sent along with it
 * (device_usbfs.go reads it). Output lines go to onLine as they come.
 * Closing the core's stdin cancels an install (--cancel-on-eof).
 */
class CoreRun(
    context: Context,
    private val args: List<String>,
    private val hand: Handoff,
    private val logFile: File? = null,
) {
    private val exe = File(context.applicationInfo.nativeLibraryDir, "libpmusb.so").path
    private val name = "pmusb-" + UUID.randomUUID()
    private val server = LocalServerSocket(name)
    @Volatile private var served = false
    private val proc: Process = ProcessBuilder(listOf(exe) + args)
        .redirectErrorStream(true)
        .also { it.environment()["PMUSB_FD_SOCKET"] = name }
        .start()

    init {
        thread(name = "pmusb-handoff", isDaemon = true) { serve() }
    }

    private fun serve() {
        try {
            server.accept().use { s ->
                if (served) return
                val fds = mutableListOf<FileDescriptor>()
                val words = mutableListOf<String>()
                hand.usb?.let { u ->
                    words += "usb=${fds.size}"
                    fds += u.fd
                    words += "in=0x%02x".format(u.epIn)
                    words += "out=0x%02x".format(u.epOut)
                    words += "iface=${u.iface}"
                    if (u.serial.isNotEmpty()) words += "serial=${u.serial}"
                }
                hand.iso?.let {
                    words += "iso=${fds.size}"
                    fds += it.fileDescriptor
                }
                s.setFileDescriptorsForSend(fds.toTypedArray())
                s.outputStream.write((words.joinToString(" ") + "\n").toByteArray())
                s.outputStream.flush()
                served = true
            }
        } catch (_: Exception) {
            // The core ended without asking for anything.
        }
    }

    /** Reads the core's output to the end, then returns its exit code.
     *  Every line also goes to the system log (tag "pmusb"), so a failure
     *  can be read with logcat as well as in the window's Details. */
    fun run(onLine: (String) -> Unit): Int {
        Log.i(TAG, "run: " + args.joinToString(" "))
        val saved = try {
            logFile?.bufferedWriter()
        } catch (_: Exception) {
            null
        }
        saved?.appendLine("run: " + args.joinToString(" "))
        proc.inputStream.bufferedReader().forEachLine {
            if (!it.contains("\"event\":\"progress\"")) {
                Log.i(TAG, it)
                saved?.appendLine(it)
                saved?.flush()
            }
            onLine(it)
        }
        val rc = proc.waitFor()
        Log.i(TAG, "exit $rc")
        saved?.appendLine("exit $rc")
        saved?.close()
        // accept() does not return when the server socket is closed, so a
        // core that never connected is answered by a connection of our own.
        if (!served) {
            served = true
            try {
                LocalSocket().use { it.connect(LocalSocketAddress(name)) }
            } catch (_: Exception) {
            }
        }
        try {
            server.close()
        } catch (_: Exception) {
        }
        return rc
    }

    companion object {
        const val TAG = "pmusb"
    }

    fun cancel() {
        try {
            proc.outputStream.close()
        } catch (_: Exception) {
        }
    }
}
