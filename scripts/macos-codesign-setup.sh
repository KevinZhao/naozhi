#!/usr/bin/env bash
# macos-codesign-setup.sh — one-time: give a macOS naozhi install a stable
# code-signing identity so privacy grants (Full Disk Access, Documents, cloud
# drives …) survive upgrades. See docs/ops/macos-codesign.md for the why.
#
# 1. Creates a self-signed code-signing identity in the login keychain (skipped
#    when one with that name exists). The private key never leaves the keychain.
# 2. Re-signs the binary with it (sign a copy, then mv: a new inode, so launchd
#    does not refuse the swapped binary with exit 78).
#
# It does NOT restart the service or grant Full Disk Access; it prints both steps.
# Later upgrades (`naozhi upgrade`, dashboard install) re-sign automatically.
#
# Usage: scripts/macos-codesign-setup.sh [path-to-naozhi]   (default: ~/.local/bin/naozhi)
# Env:
#   NAOZHI_CODESIGN_NAME        certificate common name   (default: naozhi-local)
#   NAOZHI_CODESIGN_IDENTIFIER  code identifier           (default: com.naozhi.naozhi)
set -euo pipefail

[[ "$(uname -s)" == "Darwin" ]] || { echo "macOS only" >&2; exit 1; }

BIN="${1:-$HOME/.local/bin/naozhi}"
NAME="${NAOZHI_CODESIGN_NAME:-naozhi-local}"
IDENT="${NAOZHI_CODESIGN_IDENTIFIER:-com.naozhi.naozhi}"
KEYCHAIN="$HOME/Library/Keychains/login.keychain-db"

[[ -f "$BIN" ]] || { echo "no binary at $BIN" >&2; exit 1; }
BIN="$(cd "$(dirname "$BIN")" && pwd -P)/$(basename "$BIN")"

# Already leaf-signed: keep that identifier — changing it drops the grants too.
if dr="$(codesign -d -r- "$BIN" 2>/dev/null)" &&
	[[ "$dr" =~ identifier\ \"([^\"]+)\"\ and\ certificate\ leaf ]]; then
	IDENT="${BASH_REMATCH[1]}"
fi

if security find-certificate -c "$NAME" "$KEYCHAIN" >/dev/null 2>&1; then
	echo "identity \"$NAME\" already in the login keychain"
else
	echo "creating self-signed code-signing identity \"$NAME\""
	work="$(mktemp -d)"
	trap 'rm -rf "$work"' EXIT
	pass="$(openssl rand -hex 16)"
	openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
		-keyout "$work/key.pem" -out "$work/cert.pem" -subj "/CN=$NAME" \
		-addext "basicConstraints=critical,CA:false" \
		-addext "keyUsage=critical,digitalSignature" \
		-addext "extendedKeyUsage=critical,codeSigning" 2>/dev/null
	# OpenSSL 3 defaults to a PKCS#12 cipher `security import` rejects; LibreSSL has no -legacy.
	openssl pkcs12 -export -legacy -out "$work/id.p12" -inkey "$work/key.pem" -in "$work/cert.pem" \
		-name "$NAME" -passout "pass:$pass" 2>/dev/null ||
		openssl pkcs12 -export -out "$work/id.p12" -inkey "$work/key.pem" -in "$work/cert.pem" \
			-name "$NAME" -passout "pass:$pass"
	# -T: codesign may use the key without a keychain dialog (upgrades run unattended).
	security import "$work/id.p12" -k "$KEYCHAIN" -P "$pass" -T /usr/bin/codesign >/dev/null
fi

leaf="$(security find-certificate -c "$NAME" -Z "$KEYCHAIN" | awk '/SHA-1 hash:/ {print $3; exit}')"
[[ -n "$leaf" ]] || { echo "cannot find the SHA-1 of \"$NAME\"" >&2; exit 1; }

cp -p "$BIN" "$BIN.codesign-new"
codesign -f -s "$leaf" --identifier "$IDENT" "$BIN.codesign-new"
codesign -v "$BIN.codesign-new"
mv -f "$BIN.codesign-new" "$BIN"
codesign -d -r- "$BIN" 2>&1 | grep designated

cat <<EOF

Signed. Remaining steps:
  1. Restart the service so it runs the re-signed binary:
       launchctl bootout gui/\$(id -u)/<label> && launchctl bootstrap gui/\$(id -u) ~/Library/LaunchAgents/<label>.plist
  2. System Settings → Privacy & Security → Full Disk Access → "+" → $BIN
  3. Check: naozhi doctor   (codesign should be ✓)
EOF
