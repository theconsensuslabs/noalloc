#!/bin/sh
# fuzz.sh [fuzztime] fuzzes every fuzz target in the repository for fuzztime
# (default 20s), one target at a time, and prints the input of each target
# that fails. It exits 1 when any target fails.
set -eu
fuzztime="${1:-20s}"
started="$(mktemp)"
status=0
for pkg in $(go list ./...); do
	for target in $(go test -list '^Fuzz' "$pkg" | grep '^Fuzz' || true); do
		echo "== $pkg $target"
		if ! go test -run '^$' -fuzz "^${target}\$" -fuzztime "$fuzztime" "$pkg"; then
			status=1
			find . -path "*/testdata/fuzz/$target/*" -type f -newer "$started" | while read -r input; do
				echo "-- failing input $input:"
				cat "$input"
			done
		fi
	done
done
rm -f "$started"
exit "$status"
