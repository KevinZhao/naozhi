#!/bin/sh
# Builds the naozhi-audio helper (macOS only). The Info.plist is linked into the binary: TCC reads
# the usage descriptions from it, and a bare executable needs no .app bundle once it disclaims
# responsibility (see main.swift). Usage: build.sh OUT [arm64|x86_64]
set -eu
here=$(cd "$(dirname "$0")" && pwd)
out=${1:?output path}
arch=${2:-$(uname -m)}
swiftc -O -swift-version 5 -target "$arch-apple-macos14.2" -o "$out" "$here/main.swift" \
	-Xlinker -sectcreate -Xlinker __TEXT -Xlinker __info_plist -Xlinker "$here/Info.plist"
