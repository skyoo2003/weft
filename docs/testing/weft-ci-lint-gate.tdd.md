# PR #14's failing check — TDD evidence

**Source**: no plan file. The trigger was the `make all` job on
[PR #14](https://github.com/skyoo2003/weft/pull/14) failing at its Lint step while the
same `make all` passed locally at every one of the five commits the branch added.
**Branch**: `m7-baseline`
**Date**: 2026-08-22

The defect under test is not the twelve lint findings. It is the gate: `make all` — the
target CONTRIBUTING.md and `.pre-commit-config.yaml` both present as *the* check — could
pass on a tree CI rejected, five times in a row, because nothing local read
`.golangci.yaml`. The findings are the symptom that made it visible.

A second defect sat behind it. CI's log ended `4 issues` for a tree holding twelve:
golangci-lint caps identical findings per linter at three by default. Fixing what that
log named would have failed the next run on the eight it never printed, and the run after
that on the five in `bench/` the job never reached, because it stopped at the first of its
two lint steps.

## Journey

> As a contributor, I want the gate I run before I push to judge my tree by the same
> rules CI does, so that CI tells me about my code rather than about my tooling.

Acceptance: on a tree with a lint finding, `make all` fails; the failure lists every
finding, in both modules; and a checkout without golangci-lint installed still runs the
gate.

## Cycle — the gate

No new Go test. The executable check for a Makefile target is the target, and the
assertion is a change in its exit status on a tree that did not change.

**RED**, at `a72e7e0` plus the Makefile and `.golangci.yaml` edits in `81ae3ad`, with no
Go file touched:

```console
$ make all
cmd/weft-eval/bench.go:370:14: Error return value of `fmt.Fprintf` is not checked (errcheck)
...
12 issues:
* errcheck: 11
* lll: 1
make[1]: *** [lint] Error 1
make: *** [lint-if-present] Error 2
```

The same `make all` at `a72e7e0` exits 0. That difference is the fix; the twelve are what
it was blind to. Twelve rather than the four CI printed is the second defect's fix,
visible in the same output.

**GREEN**, after `889fcee`:

```console
$ make all
golangci-lint run ./...
0 issues.
cd bench && golangci-lint run ./...
0 issues.
$ echo $?
0
```

The `bench/` line is the part CI had never run. Its five findings were pre-existing and
are fixed in `889fcee` too.

### The skip path, which the first version of it got wrong

Running the branch is what caught it. Written as two recipe lines — a `command -v` guard
with `exit 0`, then the lint — `lint-if-present` printed SKIP and then ran the lint it had
just said it was skipping:

```console
$ env PATH="$tmp:/usr/bin:/bin" make lint-if-present   # golangci-lint not on this PATH
SKIP: golangci-lint is not installed — CI will lint this. 'make lint' says how to install it.
FAIL: golangci-lint is not installed.
make: *** [lint-if-present] Error 2
skip exit=2
```

Make gives each recipe line its own shell, so the `exit 0` skipped nothing after it —
the trap `deps` and `recall` in the same file already carry a comment about. That is a
fresh checkout with no golangci-lint failing `make all`, which is the one property this
target exists to preserve. Rewritten as a single `if`, all three branches now hold:

```console
$ env PATH="$tmp:/usr/bin:/bin" make lint-if-present   # tool absent
SKIP: golangci-lint is not installed — CI will lint this. 'make lint' says how to install it.
skip exit=0

$ make lint-if-present                                  # tool present, tree clean
0 issues.
present exit=0

$ make lint-if-present                                  # tool present, one planted finding
make: *** [lint-if-present] Error 2
propagate exit=2
```

The third used a throwaway file calling `(*os.File).WriteString` without checking it,
deleted after the run: `if` reports the status of the branch it took, and a wrapper that
swallowed a lint failure would disable the gate while looking like it was running.

`make lint`, asked for by name, still fails on a missing binary and prints the install
command — a silent skip is the wrong answer to a direct request.

## Cycle — the twelve findings

Behaviour-preserving by construction, and already covered: `cmd/weft-eval/bench_test.go`
and `bench/summary_test.go` assert the printed text of both summaries, which is what a
change that only moves where a call lives has to leave alone. The split `-rates` usage
string is two Go literals concatenating to the byte sequence that was there before.

```console
$ go test ./cmd/weft-eval/ && cd bench && go test ./...
ok  github.com/skyoo2003/weft/cmd/weft-eval  1.574s
ok  github.com/skyoo2003/weft/bench          0.807s
```

## What the passing gate guarantees

| # | What is guaranteed | Test or command | Type | Result |
|---|--------------------|-----------------|------|--------|
| 1 | A lint finding in either module fails `make all` locally, not only in CI | `make all` (RED above, GREEN after fix) | gate | PASS |
| 2 | A lint run reports every finding, not three per linter | `make lint` printing 12 where CI printed 4 | gate | PASS |
| 3 | A checkout without golangci-lint still exits 0 on `make all` | `make lint-if-present` on a `PATH` without the binary | gate | PASS |
| 4 | The wrapper does not swallow a lint failure | `make lint-if-present` with a planted finding | gate | PASS |
| 5 | `make lint`, asked for by name, still refuses to pass without the tool | `make lint` with the binary hidden | gate | PASS |
| 6 | Both summaries print what they printed before the refactor | `cmd/weft-eval/bench_test.go`, `bench/summary_test.go` | unit | PASS |
| 7 | Every `.go` file still carries its SPDX header | `make spdx` | gate | PASS |

## Coverage and gaps

| Package | Before (`a72e7e0`) | After |
|---------|--------------------|-------|
| `cmd/weft-eval` | 45.0% | 45.1% |
| `bench` | 10.9% | 11.3% |

Both are below the 80% an ECC TDD cycle asks for, and this cycle did not go after that.
Neither figure moved by more than the wrapper itself: the change adds no behaviour to
test, and the statements these packages leave uncovered are the index build, the load
driver and the bleve calls — paths that need the multi-gigabyte corpus `make eval-data`
downloads, which is why they are exercised by `make bench` on a quiet machine rather than
by `go test`.

Three things this cycle deliberately does not do:

- **Version skew is warned about, not pinned.** `make lint` says so when the local binary
  is not the `GOLANGCI_VERSION` CI installs — 2.13.1 against 2.12.2 here. A local pass is
  now strong evidence of a CI pass, not a proof of one.
- **No changelog entry.** `changes/` feeds a changelog for people who consume the library;
  nothing here changes what weft does. `make changelog-check` still passes.
- **`make lint-docs` and `make fuzz` stay out of `all`.** Same reason as before: a tool
  this repository cannot assume, and a minute of wall clock. CI runs both.

## Merge evidence

Three commits on `m7-baseline`, RED then GREEN then the defect the skip path had:

- `81ae3ad` — the gate that only CI ran now runs locally, and reports all of what it
  finds. Carries the RED output above.
- `889fcee` — eleven report lines drop their error in one place, and the flag usage fits.
  Carries the GREEN output above.
- the commit adding this file — the skip branch rewritten as one shell, plus the two
  documents that described a gate that had changed under them.

If they are squashed, this file is the surviving record of which was which.
