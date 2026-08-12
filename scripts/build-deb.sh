#!/bin/sh
set -eu

if [ "$#" -ne 4 ]; then
  echo "usage: build-deb.sh BINARY VERSION ARCH OUTPUT" >&2
  exit 2
fi

binary=$1
version=$2
architecture=$3
output=$4

case "$version" in
  ""|*[!A-Za-z0-9._+-]*)
    echo "version contains unsupported Debian package characters" >&2
    exit 2
    ;;
esac
case "$architecture" in
  amd64|arm64) ;;
  *) echo "unsupported Debian architecture: $architecture" >&2; exit 2 ;;
esac

source_root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM
control_root="$temporary/control"
data_root="$temporary/data"
mkdir -p "$control_root" "$data_root/usr/bin" "$data_root/usr/lib/systemd/user" \
  "$data_root/usr/share/doc/edukod-companion" "$(dirname "$output")"

install -m 0755 "$binary" "$data_root/usr/bin/edukod-companion"
install -m 0644 "$source_root/packaging/systemd/edukod-companion.service" \
  "$data_root/usr/lib/systemd/user/edukod-companion.service"
install -m 0644 "$source_root/README.md" "$data_root/usr/share/doc/edukod-companion/README.md"
install -m 0644 "$source_root/LICENSE" "$data_root/usr/share/doc/edukod-companion/copyright"
printf '%s\n' \
  'Package: edukod-companion' \
  "Version: $version" \
  'Section: utils' \
  'Priority: optional' \
  "Architecture: $architecture" \
  'Maintainer: EduKod <https://edukod.cz>' \
  'Homepage: https://github.com/edukod-cz/edukod-companion' \
  'Recommends: libsecret-tools' \
  'Description: Secure EduKod bridge to a loopback OpenAI-compatible model' \
  ' The companion creates an outbound WSS connection and never exposes a local listener.' \
  >"$control_root/control"

epoch=${SOURCE_DATE_EPOCH:-0}
tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$epoch" \
  -C "$control_root" -cf - ./control | gzip -n -9 >"$temporary/control.tar.gz"
tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$epoch" \
  -C "$data_root" -cf - ./usr | gzip -n -9 >"$temporary/data.tar.gz"
printf '2.0\n' >"$temporary/debian-binary"

output_absolute=$(CDPATH= cd -- "$(dirname "$output")" && pwd)/$(basename "$output")
(
  cd "$temporary"
  ar rcsD "$output_absolute" debian-binary control.tar.gz data.tar.gz
)
