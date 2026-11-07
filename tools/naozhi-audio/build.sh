#!/bin/sh
# Builds the naozhi-audio helper (macOS only) into OUT. The Info.plist is linked into the binary:
# TCC reads the usage descriptions from it, and a bare executable needs no .app bundle once it
# disclaims responsibility (see Sources/naozhi-audio/main.swift). Usage: build.sh OUT [arm64|x86_64]
set -eu
here=$(cd "$(dirname "$0")" && pwd)
out=${1:?output path}
arch=${2:-$(uname -m)}
cd "$here"
swift build -c release --arch "$arch" --product naozhi-audio
cp "$(swift build -c release --arch "$arch" --show-bin-path)/naozhi-audio" "$out"
