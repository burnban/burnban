#!/bin/sh
# Collect the exact root license/notice files for modules linked into Burnban.
set -eu

DEST=${1:-third_party_licenses}
case "$DEST" in
  ""|/|.) echo "collect_licenses.sh: unsafe destination: $DEST" >&2; exit 2 ;;
esac
case "$(basename "$DEST")" in
  licenses|third_party_licenses) ;;
  *) echo "collect_licenses.sh: destination must end in licenses or third_party_licenses: $DEST" >&2; exit 2 ;;
esac

MARKER=.burnban-generated-license-bundle
if [ -e "$DEST" ]; then
  if [ ! -f "$DEST/$MARKER" ] ||
     ! grep -Fqx 'burnban-generated-license-bundle-v1' "$DEST/$MARKER"; then
    echo "collect_licenses.sh: refusing to replace unmarked destination: $DEST" >&2
    exit 2
  fi
fi

PARENT=$(dirname "$DEST")
mkdir -p "$PARENT"
BUILD=$(mktemp -d "$PARENT/.burnban-licenses.XXXXXX")
trap 'rm -rf "$BUILD"' EXIT HUP INT TERM

go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}}{{end}}{{end}}' . |
  sed '/^$/d' | sort -u |
  while IFS= read -r module; do
    case "$module" in
      /*|*../*|*/..|..) echo "collect_licenses.sh: unsafe module path: $module" >&2; exit 1 ;;
    esac
    dir=$(go list -m -f '{{.Dir}}' "$module")
    target="$BUILD/$module"
    mkdir -p "$target"
    found=0
    for candidate in "$dir"/LICENSE* "$dir"/COPYING* "$dir"/NOTICE*; do
      [ -f "$candidate" ] || continue
      cp "$candidate" "$target/$(basename "$candidate")"
      found=1
    done
    if [ "$found" -ne 1 ]; then
      echo "collect_licenses.sh: no license file found for $module" >&2
      exit 1
    fi
  done

mkdir -p "$BUILD/github.com/burnban/burnban"
cp LICENSE "$BUILD/github.com/burnban/burnban/LICENSE"
printf '%s\n' 'burnban-generated-license-bundle-v1' > "$BUILD/$MARKER"

rm -rf "$DEST"
mv "$BUILD" "$DEST"
trap - EXIT HUP INT TERM
