#!/usr/bin/env bash
# Diverse double-compiling (Wheeler) for the Resid compiler.
#
# The committed seed (build/boot/seed.ll) is the compiler's IR as the
# compiler produced it from its own source. Here the same source is run by
# residi, an interpreter written independently in Go, on that same source.
# If the IR it emits is byte-identical to the seed (and the runtime IR to
# build/boot/rt.ll), the seed is what the source says it is: a trojan in
# the seed would have to be in this interpreter too, or in Go.
#
# Usage: scripts/ddc.sh RESID_CHECKOUT
set -euo pipefail
RES="$(realpath "${1:?usage: scripts/ddc.sh RESID_CHECKOUT}")"
DDC="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

(cd "$DDC" && go build -o "$WORK/residi" ./cmd/residi)
export RESID_HOME="$RES/build/boot"
# The key only signs the linked binary, which is never made here.
export RESID_SIGNING_KEY="${RESID_SIGNING_KEY:-$RES/keys/resid-ed25519.key}"
if [ ! -f "$RESID_SIGNING_KEY" ]; then
    printf '%s\n' "$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')" > "$WORK/key"
    export RESID_SIGNING_KEY="$WORK/key"
fi

ok=0
step() { printf '\n== %s\n' "$*"; }

step "compiler: residi runs compiler/driver.resid on itself"
start=$(date +%s)
(cd "$RES" && "$WORK/residi" --stop-at-exec "$RES/compiler/driver.resid" \
    "$RES/compiler/driver.resid" -o "$WORK/stage" --runtime-internals > "$WORK/stage.log" 2>&1) \
    || { tail -20 "$WORK/stage.log"; exit 1; }
echo "   $(( $(date +%s) - start ))s"
if cmp -s "$WORK/stage.ll" "$RES/build/boot/seed.ll"; then
    echo "   IDENTICAL to build/boot/seed.ll ($(sha256sum < "$WORK/stage.ll" | cut -c1-16))"
else
    echo "   DIFFERS from build/boot/seed.ll"; diff "$WORK/stage.ll" "$RES/build/boot/seed.ll" | head -20; ok=1
fi

step "runtime: residi lowers runtime/rt/rt.resid"
start=$(date +%s)
(cd "$RES" && "$WORK/residi" --stop-at-exec "$RES/compiler/driver.resid" \
    runtime/rt/rt.resid --runtime-module -o "$WORK/rt" > "$WORK/rt.log" 2>&1) \
    || { tail -20 "$WORK/rt.log"; exit 1; }
echo "   $(( $(date +%s) - start ))s"
if cmp -s "$WORK/rt.ll" "$RES/build/boot/rt.ll"; then
    echo "   IDENTICAL to build/boot/rt.ll ($(sha256sum < "$WORK/rt.ll" | cut -c1-16))"
else
    echo "   DIFFERS from build/boot/rt.ll"; diff "$WORK/rt.ll" "$RES/build/boot/rt.ll" | head -20; ok=1
fi
exit $ok
