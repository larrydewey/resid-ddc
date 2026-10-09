# resid-ddc

Diverse double-compiling (David A. Wheeler, 2009) for the
[Resid](https://github.com/larrydewey/Resid) compiler.

Resid's compiler is self-hosted: `build/boot/seed.ll` is the compiler's
LLVM IR, produced by the compiler from its own source, and `boot.sh` checks
that it reaches a fixed point. A fixed point by itself does not rule out a
"trusting trust" attack, where a compromised seed recognises the compiler's
source and inserts itself again. DDC rules it out by producing the same
artifact a second way, with no code from the first.

`residi` is that second way: an interpreter for the subset of Resid the
compiler is written in. It is written from scratch in Go and shares no code
with the Resid toolchain. It runs `compiler/driver.resid` on its own source.
If the IR it writes is byte-identical to `build/boot/seed.ll`, the seed is
exactly what the source says it is. A trojan would have to be in the seed,
in this interpreter and in the Go toolchain all at once.

The same holds for the runtime: `residi` lowers `runtime/rt/rt.resid` and
the result must equal `build/boot/rt.ll`.

## Run it

```sh
scripts/ddc.sh ../resid        # path to a Resid checkout
```

```
== compiler: residi runs compiler/driver.resid on itself
   30s
   IDENTICAL to build/boot/seed.ll (d4d35c4f5457e5f5)

== runtime: residi lowers runtime/rt/rt.resid
   7s
   IDENTICAL to build/boot/rt.ll (f6a82dab92ebe84e)
```

That run used Resid at `7e8fc3f` and Go 1.27. The script exits nonzero if
either artifact differs.

## What is trusted

- **Go**: the toolchain that builds `residi`, and the Go runtime.
- **clang/LLVM**: it turns the verified `.ll` into machine code, here as in
  `boot.sh`. DDC verifies the IR, not the binary. A second, independent
  LLVM (or a second C compiler building clang) would extend the check down
  to the binary.
- **The source**: DDC shows the seed matches the source. It does not show
  the source is benign. That still takes review.

## How the interpreter works

- `interp/lex.go`, `parse.go`: a lexer and parser for the language, built
  from Resid's own grammar (`compiler/graph.resid`) but with no code taken
  from it.
- `interp/load.go`: modules and imports. An imported module's private
  functions stay private and the main file's names win, as in the
  compiler's module merge.
- `interp/compile.go`: each function compiles once to Go closures over a
  frame of slots. Frames are windows of a segmented value stack. A
  `return f(...)` in tail position (also through `if`, ternaries and block
  expressions) reuses the frame, because Resid loops are tail recursion.
- `interp/value.go`, `num.go`: values. Integers of any declared width are
  exact (`math/big` beyond 64 bits), with checked, wrapping and saturating
  arithmetic. Strings are indexed by codepoint. Lists are immutable but
  append in place for the newest list over a backing array, which keeps
  the compiler's `acc.concat([x])` loops linear. `+` on strings does the
  same.
- `interp/hamt.go`: maps and sets as a persistent hash trie. It iterates in
  the Resid runtime's canonical order (FNV-1a 64 of the key's text, ordered
  by 5-bit chunks, lowest chunk first). The compiler's output depends on
  that order.
- `interp/fmt.go`: f-string format specs and float text. These are written
  from the definitions in `runtime/rt/fmt.resid` and C's `%g`, plus Unicode
  case mapping from Go's tables and `SpecialCasing.txt`
  (`special_casing.go`).
- `interp/builtins.go`: the `str_*` builtins, providers (`filesystem`,
  `environment`, `args`, `process`), builders and methods. With
  `--stop-at-exec`, the program stops when the compiler first runs
  `clang`, after it has written its `.ll`.

Region and arena controls (`resid_arena_*`, `resid_bulk_*`) do nothing
here, because Go collects garbage. The raw-memory builtins are not
supported. Only the language server (`residc lsp`) uses them, and DDC never
runs it.

## Cross-checking on the conformance suite

```sh
scripts/compare-cases.sh ../resid [REGEX]
```

This compiles every case in `tests/conformance/cases` twice: once with the
native compiler and once with the compiler running under `residi`. It
compares the IR, or the diagnostics when compilation fails. At `7e8fc3f`,
395 of 401 cases agree. The other six:

- **The reducer's memory budget (5 cases):** `dt_calendar`,
  `loop_regions_lists`, `reduce_known_names`, `scalar_scopes` and
  `regex_basics`. Compile-time reduction stops after 512 MB of *native*
  allocation (`rd_max_mem` in `compiler/reduce.resid`, measured by
  `resid_mem_since_mark`). That is a property of the native runtime that an
  independent implementation cannot reproduce byte for byte. `residi`
  never spends the budget, so it folds more. A native compiler built with
  the budget lifted produces the interpreter's IR for the first three. The
  other two exhaust native's 4 GB limit without the budget. The
  self-compile never reaches the budget, which is why its IR matches.
- **`unicode_case_mapping`:** a real discrepancy, found by this check. The
  runtime's hand-written SpecialCasing table (`tools/gen_case_tables.py`)
  has wrong entries for U+1F90–1F9F, U+1FA0–1FAF and U+1FB2–1FFC. For
  example, `str_to_upper("ᾨ")` gives `ᾨΙ` where Unicode gives `ὨΙ`.
  `residi` follows Unicode.

## Diagnostics

- `residi --check prog.resid` compiles every function eagerly and reports
  unknown names or unsupported constructs.
- `RESIDI_CPUPROFILE=f` (with `RESIDI_PROFSECS=n`) writes a Go CPU profile.
- `RESIDI_RPROF=1` samples which Resid functions are running and prints the
  totals on SIGINT. `RESIDI_COUNT=1` prints call counts.
