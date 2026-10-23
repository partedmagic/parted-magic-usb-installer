# Parted Magic USB Installer

Writes [Parted Magic](https://partedmagic.com/) to a USB flash drive.

The stick it makes is one FAT32 partition that boots on legacy BIOS and on
UEFI with Secure Boot on. Every file is read back and checked before the
installer reports success. The partition table is written last, so a stick
that fails partway has no partition table and is never left half bootable.
The stick stays writable, so Parted Magic's Save Session works on it.

It runs on:

- Windows 10 and 11: `pmusb-gui.exe`, in the `install` folder of the
  Parted Magic ISO
- Linux: packages in the same folder (`.deb`, `.rpm`, Slackware `.txz`)
- macOS on Apple silicon: an app (in testing)
- Android phones and tablets with USB host support: an app (in testing)

## How it is built

One program in Go does the work on every system. It reads the ISO, lays out
the drive, writes the FAT32 filesystem, installs the BIOS boot loader and
checks the result. Parted Magic 26.10_4 and later boot BIOS computers with
GRUB: the installer copies GRUB's boot.img and core.img from the ISO itself
into the first sectors of the drive. Older ISOs get syslinux, as before. Each system adds a small part that finds and opens USB
drives:

| file | system | how it reaches the drive |
|---|---|---|
| `device_linux.go` | Linux | sysfs and the block device |
| `device_windows.go` | Windows | the physical drive handle |
| `device_darwin.go` | macOS | diskutil, and the raw disk opened through `authopen` |
| `device_usbfs.go` | Android | an Android app cannot open a disk, so this file speaks USB mass storage (Bulk-Only Transport and SCSI) over the descriptor Android's USB API gives the app |

The windows are `gui_windows.go` (Windows), `pmusb-gtk.py` (Linux, GTK 3),
`mac/` (Swift, AppKit) and `android/` (Kotlin, Jetpack Compose). Each one
runs the same program and reads its progress as JSON lines.

## Building

Go 1.26 or later. The Go dependencies are in `vendor/`.

- `./build.sh`: the Linux program and the Windows executables
- `mac/build-mac.sh CORE OUTDIR`: on a Mac with the Command Line Tools.
  `CORE` is the program built with
  `GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build`.
- `android/build-android.sh`: needs JDK 17 and the Android SDK
  (`JAVA_HOME`, `ANDROID_HOME`). It builds the program for android/arm64
  and puts it in the app.

## Command line

    pmusb inspect --source pmagic.iso
    pmusb list-devices [--all] [--fixed]
    pmusb install --source ISO --target /dev/sdX --confirm-erase [--expect "model|serial|bytes"]
                  [--label PMAGIC] [--no-verify] [--no-bios] [--json] [--cancel-on-eof]
    pmusb install --source ISO --target stick.img --size 3G      (image file, for testing)
    pmusb verify --target /dev/sdX|stick.img
    pmusb upgrade --source ISO --target /dev/sdX --confirm-upgrade [--delete-session]   (Linux)

Exit codes: 0 ok, 2 usage, 3 refused by a safety check, 4 device error,
5 verify failed, 6 canceled.

## License

The installer is GPL-2.0-or-later (`COPYING`). It includes syslinux's
installer code and boot loader, also GPL-2.0-or-later. It contains no GRUB
code: GRUB's boot images are read from the Parted Magic ISO. The Android app also
uses AndroidX and Jetpack Compose, which are Apache-2.0, so the Android app
as a whole is distributed under GPL-3.0-or-later (`android/LICENSE`). The
third-party Go libraries are listed in `THIRD-PARTY-NOTICES.txt`.

Copyright 2026 Patrick J. Verner.
