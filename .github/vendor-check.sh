#!/bin/sh
# vendor-check.sh checks encoding/json/internal/strconv against src/internal/strconv
# of the Go toolchain running it: each file Go has, with upstream.patch
# applied, is byte for byte ours; ours holds no other Go file but
# shortest.go; and LICENSE is Go's. A Go release that changes that source
# fails the check until the copy is taken again.
set -eu
ours="$(cd "$(dirname "$0")/../encoding/json/internal/strconv" && pwd)"
goroot="$(go env GOROOT)"
upstream="$goroot/src/internal/strconv"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
status=0
for f in "$upstream"/*.go; do
	case "$(basename "$f")" in
	*_test.go | pow10gen.go) ;;
	*) cp "$f" "$work/" ;;
	esac
done
(cd "$work" && patch -s -p1 <"$ours/upstream.patch")
for f in "$work"/*.go; do
	name="$(basename "$f")"
	if ! cmp -s "$f" "$ours/$name"; then
		echo "$name differs from $(go version | cut -d' ' -f3)'s internal/strconv with upstream.patch applied:" >&2
		diff -u "$f" "$ours/$name" >&2 || true
		status=1
	fi
done
for f in "$ours"/*.go; do
	name="$(basename "$f")"
	if [ "$name" != shortest.go ] && [ ! -f "$work/$name" ]; then
		echo "$name is not in Go's internal/strconv" >&2
		status=1
	fi
done
if ! cmp -s "$goroot/LICENSE" "$ours/LICENSE"; then
	echo "LICENSE is not Go's" >&2
	status=1
fi
exit "$status"
