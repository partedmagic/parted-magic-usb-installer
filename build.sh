#!/bin/sh
# Builds the three binaries. Needs Go 1.26 on PATH (host: /usr/lib64/go1.26.5/go/bin).
set -e
cd "$(dirname "$0")"
LD='-s -w'
go build -trimpath -ldflags="$LD" -o pmusb .
# Windows GUI: manifest (requireAdministrator, Common Controls 6) from winres/winres.json
go run github.com/tc-hib/go-winres@v0.3.3 make --in winres/winres.json --out winres/rsrc
cp winres/rsrc_windows_amd64.syso ./rsrc_windows_amd64.syso
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -tags gui -trimpath -ldflags="$LD -H windowsgui" -o pmusb-gui.exe .
rm -f ./rsrc_windows_amd64.syso
# Windows command-line build, no manifest (asInvoker)
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="$LD" -o pmusb.exe .
sha256sum pmusb pmusb-gui.exe pmusb.exe
