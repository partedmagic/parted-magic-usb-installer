// Copyright 2026 Patrick J. Verner
// SPDX-License-Identifier: GPL-3.0-or-later

package com.partedmagic.usbinstaller

import android.hardware.usb.UsbConstants
import android.hardware.usb.UsbDevice
import android.hardware.usb.UsbDeviceConnection
import android.hardware.usb.UsbInterface
import android.hardware.usb.UsbManager
import android.os.ParcelFileDescriptor
import java.io.FileDescriptor

/** USB sticks as Android's USB API sees them. */
object UsbDrive {
    const val ACTION_PERMISSION = "com.partedmagic.usbinstaller.USB_PERMISSION"

    /** A drive the user allowed, with its mass-storage interface claimed,
     *  which takes it away from Android's own storage driver. */
    class Open(
        val device: UsbDevice,
        private val conn: UsbDeviceConnection,
        private val intf: UsbInterface,
        val epIn: Int,
        val epOut: Int,
        val serial: String,
    ) {
        private val pfd: ParcelFileDescriptor = ParcelFileDescriptor.fromFd(conn.fileDescriptor)
        val fd: FileDescriptor get() = pfd.fileDescriptor
        val iface: Int get() = intf.id

        fun close() {
            try {
                pfd.close()
            } catch (_: Exception) {
            }
            try {
                conn.releaseInterface(intf)
            } catch (_: Exception) {
            }
            conn.close()
        }
    }

    /** The Bulk-Only SCSI interface every USB stick has. */
    fun massStorage(dev: UsbDevice): UsbInterface? =
        (0 until dev.interfaceCount).map(dev::getInterface).firstOrNull {
            it.interfaceClass == UsbConstants.USB_CLASS_MASS_STORAGE &&
                it.interfaceSubclass == 6 && it.interfaceProtocol == 0x50
        }

    fun list(m: UsbManager): List<UsbDevice> =
        m.deviceList.values.filter { massStorage(it) != null }.sortedBy { it.deviceName }

    fun label(d: UsbDevice): String =
        listOfNotNull(d.manufacturerName, d.productName).joinToString(" ").trim()
            .ifEmpty { "USB drive" }

    fun open(m: UsbManager, d: UsbDevice): Open? {
        val intf = massStorage(d) ?: return null
        val conn = m.openDevice(d) ?: return null
        if (!conn.claimInterface(intf, true)) {
            conn.close()
            return null
        }
        var epIn = 0
        var epOut = 0
        for (i in 0 until intf.endpointCount) {
            val e = intf.getEndpoint(i)
            if (e.type != UsbConstants.USB_ENDPOINT_XFER_BULK) continue
            if (e.direction == UsbConstants.USB_DIR_IN) epIn = e.address else epOut = e.address
        }
        if (epIn == 0 || epOut == 0) {
            conn.releaseInterface(intf)
            conn.close()
            return null
        }
        val serial = try {
            d.serialNumber ?: ""
        } catch (_: SecurityException) {
            ""
        }
        return Open(d, conn, intf, epIn, epOut, serial.replace(Regex("\\s+"), "_"))
    }
}
