#!/bin/sh
set -eu

if [ "$#" -ne 4 ]; then
  echo "usage: build.sh OUTPUT VERSION GOOS GOARCH" >&2
  exit 2
fi

output=$1
version=$2
target_os=$3
target_arch=$4
go_binary=${GO_BIN:-go}

case "$output" in
  /*) ;;
  *) output=$(pwd)/$output ;;
esac

case "$version" in
  ""|*[!A-Za-z0-9._+-]*)
    echo "version contains unsupported characters" >&2
    exit 2
    ;;
esac

source_root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM

version_source="$temporary/version.go"
overlay="$temporary/overlay.json"
printf 'package app\n\nvar Version = "%s"\n' "$version" >"$version_source"
printf '{"Replace":{"%s/internal/app/version.go":"%s"}}\n' \
  "$source_root" "$version_source" >"$overlay"

mkdir -p "$(dirname "$output")"
cd "$source_root"
CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" \
  "$go_binary" build -trimpath -overlay "$overlay" -o "$output" ./cmd/edukod-companion
