# TDD evidence — milestone 3

**Source plan**: `.claude/plans/weft-m3.plan.md` (M3 — 규모)
**Branch**: `m3-scale`
**Status**: in progress. Task 1 is partly landed; tasks 1d, 1e and 2–8 are open.

This file is an index, not a substitute for the tests. It records what each
test proves and preserves that across session restarts and squash merges —
without it, a squashed branch answers "what was verified, and how" with a diff.

## User journeys

Taken from the plan rather than invented; the plan's pass lines already state
them as guarantees.

1. As an embedder with a corpus larger than memory, I want `Open` to return
   without reading the whole index, so that opening costs the same at 8× the
   corpus.
2. As an embedder, I want a lazily loaded index to rank **identically** to an
   eagerly loaded one, so that the storage change is invisible to results.
3. As an embedder, I want a commit after adding one document to write bytes
   proportional to that document, not to the corpus.
4. As a maintainer, I want the same corpus to produce byte-identical segments
   on every build, so that a measurement taken on one build means something on
   the next (the milestone 4 §4.2 failure, arriving upstream this time).
5. As a scorer author, I want the six read APIs to keep their signatures, so
   that scale costs `pkg/scorer` and `pkg/fusion` nothing.

## Task report

### Task 1 (partial) — format v2: `docoff` and `keys` seek tables

**Summary.** Version 1 wrote documents as a bare run of variable-length records
and rebuilt `byKey` by reading all of them, so neither a `DocID` nor a `Key`
could reach its document without decoding every document in front of it. Two
sections fix that, both the same shape: a fixed-width `uint64` offset table,
then the variable-length entries it points at.

**RED** — commit `70dd1f5`.

```
$ go test ./pkg/engine/ -run 'TestVersionOneIsRefused|TestDocOffsets|TestKeysSection|TestDocOffsetTable'
pkg/engine/formatv2_test.go:88:29: undefined: docoffFile
pkg/engine/formatv2_test.go:88:41: undefined: kindDocoff
pkg/engine/formatv2_test.go:89:15: undefined: parseDocOffsets
pkg/engine/formatv2_test.go:107:16: undefined: decodeDocRecord
pkg/engine/formatv2_test.go:129:30: undefined: keysFile
pkg/engine/formatv2_test.go:129:40: undefined: kindKeys
pkg/engine/formatv2_test.go:130:13: undefined: parseKeyTable
FAIL	github.com/skyoo2003/weft/pkg/engine [build failed]
```

Compile-time RED. Every failure names a symbol these tests newly reference and
nothing implements — not unrelated syntax errors, not broken setup.

**GREEN** — commit `89846d4`.

```
$ go test ./...
ok  	github.com/skyoo2003/weft/pkg/engine	3.839s
(all 10 packages ok)

$ make all       # fmt + build + vet + test -race
$ make arch      # milestone 1 assertions
$ make deps      # external dependencies: this module only
all green

$ go test ./pkg/engine/ -run=NONE -fuzz FuzzParseSection -fuzztime 20s
5,466,890 execs, no panic

$ git diff --stat main -- pkg/scorer pkg/fusion
(no output)
```

**Deliberate test edits, both forced by the version bump and neither a
weakening.** `TestOverlongVersionEncodingIsRefused` spelled version 1 as
`0x81 0x00`; at v2 that is a *wrong version*, so the version check fired first
and the assertion silently became a different one. It now derives its bytes
from `formatVersion`. `TestOtherVersionsAreRefusedNotMisread` patched to
version 2, which is now current; it patches `formatVersion + 1`. Recorded here
because "the test changed" and "the test got weaker" look alike in a diff.

### Task 1d — per-unit checksums

**Summary.** The frame checksum covers a whole file, so computing it costs a
full read — the cost this milestone removes. A reader that maps a segment and
touches one record never computes it, so integrity has to be checkable one unit
at a time or it is not checkable at all.

**RED** — commit `eab4750`. The sweep measured rather than merely failed:

```
$ go test ./pkg/engine/ -run 'TestEveryByteFlipIsCaughtWithoutTheFrameCRC|TestARecordDecodedUnderTheWrongIDIsRefused'
--- FAIL: TestEveryByteFlipIsCaughtWithoutTheFrameCRC/docs
    docs payload byte 10 flipped, frame checksum repaired: got <nil>, want ErrCorrupt
    docs payload byte 11 flipped, frame checksum repaired: got <nil>, want ErrCorrupt
    docs payload byte 12 flipped, frame checksum repaired: got <nil>, want ErrCorrupt
    docs payload byte 26 flipped, frame checksum repaired: got <nil>, want ErrCorrupt
--- FAIL: TestEveryByteFlipIsCaughtWithoutTheFrameCRC/terms
    terms payload byte 8 flipped, frame checksum repaired: got <nil>, want ErrCorrupt
--- FAIL: TestARecordDecodedUnderTheWrongIDIsRefused
    document 2's record ("charlie") decoded as document 1 returned "charlie", <nil>; want ErrCorrupt
```

Bytes 10–12 and 26 are the two documents' text; terms byte 8 is the term
string. **The plan's guess was half wrong.** It named postings blocks and docs
records as the unprotected pair; postings survived 0 of its flips, because
every field in a block is cross-checked against the block's contents. The
unprotected pair is `docs` and `terms`.

Postings still got the checksum, for a reason this test cannot show while
`Open` is eager: it decodes every block, so every invariant is re-derived. The
lazy reader skips blocks, and a skipped block's invariants are re-derived by
nobody.

**GREEN** — commit `b431d1e`. All positions caught, wrong-id read refused,
`make all`/`arch`/`deps` green, `FuzzSegmentDecoding` 6,119,898 execs no panic,
`pkg/scorer` and `pkg/fusion` diff still 0 lines.

**The cost, and a trap it sprang.** 17 of the 20 handcrafted lying-file
payloads had to start carrying correct unit checksums. Computing them rather
than letting them fail is not politeness: a lie that *also* fails its checksum
still reports `ErrCorrupt`, so the case still passes while testing the checksum
instead of the rule it is named for. `TestUnsortedTermsAreRefused` did exactly
that — passed green, never reached the ordering check. It now asserts the
reason in the error message, not just the sentinel.

## Test specification

| # | What is guaranteed | Test | Type | Result | Evidence |
|---|---|---|---|---|---|
| 1 | A version 1 segment is refused with `ErrBadVersion`, never migrated or misread | `pkg/engine/formatv2_test.go:TestVersionOneIsRefused` | unit | PASS | `go test ./pkg/engine/` |
| 2 | Every `docoff` entry is the absolute file offset where that DocID's record begins, and decoding from there — with nothing before it read — yields that document | `formatv2_test.go:TestDocOffsetsLandOnRecordStarts` | unit | PASS | `go test ./pkg/engine/` |
| 3 | An out-of-range DocID reports false rather than indexing past the table | `formatv2_test.go:TestDocOffsetsLandOnRecordStarts` | unit | PASS | same |
| 4 | The `keys` section is strictly ascending and every entry agrees with the DocID `docs` assigned; `lookup` finds present keys and reports absent ones without error | `formatv2_test.go:TestKeysSectionIsSortedAndAgreesWithDocs` | unit | PASS | same |
| 5 | The offset table is fixed width, so entry *i* is reachable by arithmetic — a uvarint table would pass #2 and #4 while leaving `Doc(id)` O(id) | `formatv2_test.go:TestDocOffsetTableIsFixedWidth` | unit | PASS | same |
| 6 | An overlong encoding of the current version is `ErrCorrupt` — two byte strings may not mean one index | `segment_test.go:TestOverlongVersionEncodingIsRefused` | unit | PASS | same |
| 7 | A future version is `ErrBadVersion`, not misread | `segment_test.go:TestOtherVersionsAreRefusedNotMisread` | unit | PASS | same |
| 8 | A `docoff` or `keys` section disagreeing with `docs` is refused at `Open` rather than answering wrongly later | `seek.go:verifySeekSections`, exercised by every `Open` in the suite | unit | PASS | `go test ./...` |
| 9 | Neither the frame parser nor the decoders panic on arbitrary bytes, including the two new section kinds | `segment_test.go:FuzzParseSection` | fuzz | PASS | 5,466,890 execs |
| 10 | The storage change costs the scorers and the fuser nothing | `git diff --stat main -- pkg/scorer pkg/fusion` | architecture | PASS | empty output |
| 11 | Any byte flip in `docs`, `postings` or `terms` is caught **with the frame checksum repaired** — the state every lazy read is in | `formatv2_test.go:TestEveryByteFlipIsCaughtWithoutTheFrameCRC` | unit | PASS | `go test ./pkg/engine/` |
| 12 | A healthy record decoded under another document's id is refused, so a damaged offset table cannot produce a plausible wrong answer | `formatv2_test.go:TestARecordDecodedUnderTheWrongIDIsRefused` | unit | PASS | same |
| 13 | Unsorted terms are refused **for being unsorted**, asserted on the reason rather than the sentinel | `segment_test.go:TestUnsortedTermsAreRefused` | unit | PASS | same |
| 14 | The 20-case lying-file matrix still trips the rule each case names, not the new checksum | `segment_test.go:TestLyingFilesAreRefused` | unit | PASS | same |

## Coverage and known gaps

Coverage is not the instrument this repository uses — `make arch` and the
corruption matrices are — so no percentage is claimed here. What *is* claimed:

- **Verification is still eager.** `verifySeekSections` reads every entry on
  every `Open`, which is O(index) and precisely what task 2 has to move into
  `Scrub`. It is written as one function so that move is a call site changing,
  not a rule being dropped.
- **Per-unit checksums cover `docs`, `postings` and `terms` only.** `docoff`
  and `keys` have none, because `verifySeekSections` still re-derives both from
  `docs` on every `Open` — the check task 2 has to move. When it moves, those
  two join the sweep, and a damaged `docoff` entry is already caught by the
  id-seeded record checksum rather than by trusting the table.
- **`meta` has no per-unit checksum and needs none**: it is 19 bytes, always
  read in full, and `Open` cross-checks every field against the documents.
- **`segWriter` still buffers whole sections** (task 1e). Its own comment calls
  that fine "by construction — the section describes an index that is itself
  entirely in memory"; task 5's merge is what breaks that premise.
- **Nothing measured on the real corpus yet.** Pass line 1 requires `make eval`
  to reproduce milestone 4's five arm numbers after a v2 rebuild. Not run —
  `.eval-data/index` is still a v1 directory and rebuilding it is task 6's work.
- **The vector arithmetic in plan §1 is unaddressed and expected to stay that
  way until task 7.** Roughly 69% of the 656 MB `docs` file is vectors, and
  `scorer/vector` scans all of them per query, so lazy loading moves those
  bytes from the Go heap to the page cache without shrinking the working set.

## Merge evidence

Checkpoint commits on `m3-scale`, oldest first:

| Commit | Stage | What it proves |
|---|---|---|
| `70dd1f5` | RED | The four format v2 tests fail to compile, each on a symbol they newly reference |
| `89846d4` | GREEN | Same tests pass; `make all`/`arch`/`deps` green; 5.4M fuzz execs no panic; `pkg/scorer` and `pkg/fusion` diff 0 lines |
| `6108a01` | docs | This file |
| `eab4750` | RED | Five payload positions survive a flip once the frame checksum is repaired; a record decodes under the wrong id with no error |
| `b431d1e` | GREEN | Those positions all caught, wrong-id read refused; `make all`/`arch`/`deps` green; 6.1M fuzz execs no panic; scorer/fusion diff still 0 |

If these are squashed, this table and the two blocks above are the surviving
record.
