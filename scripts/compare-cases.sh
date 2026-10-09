#!/usr/bin/env bash
# Compile each conformance case with the native compiler and with the
# compiler interpreted by residi; report cases whose IR or diagnostics differ.
# Usage: scripts/compare-cases.sh RESID_CHECKOUT [FILTER]
set -uo pipefail
RES="$(realpath "$1")"; FILTER="${2:-}"
DDC="$(cd "$(dirname "$0")/.." && pwd)"
RESIDI="$DDC/residi"
OUT="${DDC_OUT:-$(mktemp -d)}"
export RES RESIDI OUT RESID_HOME="$RES/build/boot" RESID_SIGNING_KEY="${RESID_SIGNING_KEY:-$RES/keys/resid-ed25519.key}"
one() {
    src="$1"; name="$(basename "$src" .resid)"
    d="$OUT/$name"; rm -rf "$d"; mkdir -p "$d/n" "$d/i"
    for w in n i; do
        cp "$RES"/lib/*.resid "$d/$w/" 2>/dev/null
        cp "$RES"/tests/conformance/cases/_* "$d/$w/" 2>/dev/null
        sed "s#@TMP@#$d/$w#g" "$src" > "$d/$w/$name.resid"
        echo hi > "$d/$w/in.txt"
    done
    args=""
    [ -f "${src%.resid}.args" ] && args="$(cat "${src%.resid}.args")"
    (cd "$d/n" && timeout 120 "$RES/build/boot/stage2.bin" "$name.resid" -o prog ${args//@TMP@/$d/n} > log 2>&1; echo "exit $?" >> log)
    (cd "$d/i" && timeout 600 "$RESIDI" --stop-at-exec "$RES/compiler/driver.resid" "$name.resid" -o prog ${args//@TMP@/$d/i} > log 2>&1; echo "exit $?" >> log)
    sed -i "s#$d/[ni]#@D@#g" "$d/n/log" "$d/i/log"
    st=ok
    [ -f "$d/n/prog.ll" ] || st=noll
    if [ -f "$d/n/prog.ll" ] || [ -f "$d/i/prog.ll" ]; then
        sed "s#$d/[ni]#@D@#g" "$d/n/prog.ll" > "$d/n.ll" 2>/dev/null
        sed "s#$d/[ni]#@D@#g" "$d/i/prog.ll" > "$d/i.ll" 2>/dev/null
        cmp -s "$d/n.ll" "$d/i.ll" || st=IR
    fi
    # The interpreter stops at the link step, so compare diagnostics only
    # when neither side reached it.
    if [ ! -f "$d/n/prog.ll" ] && ! cmp -s "$d/n/log" "$d/i/log"; then st="${st/ok/}LOG"; fi
    echo "$st $name"
}
export -f one
ls "$RES"/tests/conformance/cases/*.resid | grep -v '/_' | grep -E -- "$FILTER" | xargs -P "$(nproc)" -I{} bash -c 'one {}' | sort
echo "out: $OUT" >&2
