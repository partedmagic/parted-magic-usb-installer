// Copyright 2026 Patrick J. Verner
// SPDX-License-Identifier: GPL-3.0-or-later

package com.partedmagic.usbinstaller.ui

import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color

// The Erase Monitor palette, so the two apps look like one family.
val BgDark = Color(0xFF10151B)
val BgCard = Color(0xFF171C21)
val Accent = Color(0xFF2A7FFF)
val TxtDim = Color(0xFF8A949E)
val OkGreen = Color(0xFF7FE0A7)
val BadRed = Color(0xFFFF9AA2)

@Composable
fun PMTheme(content: @Composable () -> Unit) {
    MaterialTheme(
        colorScheme = darkColorScheme(
            primary = Accent,
            background = BgDark,
            surface = BgCard,
            onBackground = Color(0xFFDDE3E8),
            onSurface = Color(0xFFDDE3E8),
        ),
        content = content,
    )
}
