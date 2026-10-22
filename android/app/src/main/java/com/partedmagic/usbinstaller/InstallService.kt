// Copyright 2026 Patrick J. Verner
// SPDX-License-Identifier: GPL-3.0-or-later

package com.partedmagic.usbinstaller

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.IBinder
import android.os.PowerManager
import androidx.core.content.ContextCompat

/**
 * Keeps the app running while a stick is written. With the screen off, an
 * app in the background is frozen or slowed (on Samsung phones within
 * seconds), and the write stalls and fails (the Fold8, 2026-10-22). A
 * foreground service, with its notification, and a partial wake lock keep
 * the process and the CPU going until the core exits. The work itself runs
 * where it always did; this service only holds the app awake and shows the
 * progress in the notification.
 */
class InstallService : Service() {
    private var wake: PowerManager.WakeLock? = null

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        ensureChannel(this)
        val n = note(this, "Writing Parted Magic to the USB drive", "Starting...", 0, true)
        try {
            if (Build.VERSION.SDK_INT >= 29) {
                startForeground(NOTE_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_CONNECTED_DEVICE)
            } else {
                startForeground(NOTE_ID, n)
            }
        } catch (e: Exception) {
            // connectedDevice needs the USB permission the app was granted;
            // dataSync is the fallback if a phone refuses it anyway.
            if (Build.VERSION.SDK_INT >= 29) {
                startForeground(NOTE_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC)
            }
        }
        if (wake == null) {
            wake = getSystemService(PowerManager::class.java)
                .newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "pmusb:install")
                .apply { acquire(2 * 60 * 60 * 1000L) }
        }
        instance = this
        return START_NOT_STICKY
    }

    override fun onDestroy() {
        wake?.let { if (it.isHeld) it.release() }
        wake = null
        instance = null
        super.onDestroy()
    }

    companion object {
        private const val CHANNEL = "install"
        private const val NOTE_ID = 1
        private const val DONE_ID = 2

        @Volatile
        private var instance: InstallService? = null
        private var lastPercent = -1

        private fun ensureChannel(c: Context) {
            val nm = c.getSystemService(NotificationManager::class.java)
            if (nm.getNotificationChannel(CHANNEL) == null) {
                nm.createNotificationChannel(
                    NotificationChannel(CHANNEL, "Writing a USB drive", NotificationManager.IMPORTANCE_LOW)
                )
            }
        }

        private fun note(c: Context, title: String, text: String, percent: Int, ongoing: Boolean): Notification {
            val open = PendingIntent.getActivity(
                c, 0, Intent(c, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP),
                PendingIntent.FLAG_IMMUTABLE,
            )
            val b = Notification.Builder(c, CHANNEL)
                .setSmallIcon(R.drawable.ic_notification)
                .setContentTitle(title)
                .setContentText(text)
                .setContentIntent(open)
                .setOngoing(ongoing)
                .setOnlyAlertOnce(true)
            if (ongoing) b.setProgress(100, percent.coerceIn(0, 100), percent < 0)
            return b.build()
        }

        fun start(c: Context) {
            lastPercent = -1
            ContextCompat.startForegroundService(c, Intent(c, InstallService::class.java))
        }

        /** Updates the notification, at most once per whole percent. */
        fun progress(c: Context, text: String, percent: Int) {
            if (instance == null || percent == lastPercent) return
            lastPercent = percent
            c.getSystemService(NotificationManager::class.java)
                .notify(NOTE_ID, note(c, "Writing Parted Magic to the USB drive", text, percent, true))
        }

        fun stop(c: Context, title: String, text: String) {
            instance?.let {
                it.stopForeground(STOP_FOREGROUND_REMOVE)
                it.stopSelf()
            }
            ensureChannel(c)
            c.getSystemService(NotificationManager::class.java)
                .notify(DONE_ID, note(c, title, text, 0, false))
        }
    }
}
