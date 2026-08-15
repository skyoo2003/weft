# TDD evidence — milestone 3

**Source plan**: `.claude/plans/weft-m3.plan.md` (M3 — 규모)
**Branch**: `m3-scale`
**Status**: tasks 1–6 and 8 complete. Task 7 (the approximate vector index) is
not started, and section "Coverage and known gaps" says what that leaves undone.

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

```text
$ go test ./pkg/engine/ -run 'TestVersionOneIsRefused|TestDocOffsets|TestKeysSection|TestDocOffsetTable'
pkg/engine/formatv2_test.go:88:29: undefined: docoffFile
pkg/engine/formatv2_test.go:88:41: undefined: kindDocoff
pkg/engine/formatv2_test.go:89:15: undefined: parseDocOffsets
pkg/engine/formatv2_test.go:107:16: undefined: decodeDocRecord
pkg/engine/formatv2_test.go:129:30: undefined: keysFile
pkg/engine/formatv2_test.go:129:40: undefined: kindKeys
pkg/engine/formatv2_test.go:130:13: undefined: parseKeyTable
FAIL    github.com/skyoo2003/weft/pkg/engine [build failed]
```

Compile-time RED. Every failure names a symbol these tests newly reference and
nothing implements — not unrelated syntax errors, not broken setup.

**GREEN** — commit `89846d4`.

```text
$ go test ./...
ok      github.com/skyoo2003/weft/pkg/engine    3.839s
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

```text
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

### Tasks 1e, 2, 3, 4, 5, 6, 8

Each ran the same cycle: a failing test naming the missing behaviour, the
minimum that made it pass, gates re-run. The table below is what the passing
tests now guarantee; the commit list at the end is the RED/GREEN evidence.

Three of the REDs measured rather than merely failed, and the measurement is the
part worth keeping:

| Task | RED | GREEN |
| --- | --- | --- |
| 1e streaming writer | `Commit` allocated 51,453,656 B for an 8,799,768 B segment (585%) | 695,536 B for 7,223,605 B (9.6%) |
| 3 router | `Open` rebuilt every document, posting list and key | 154,696 B for a 7,223,605 B segment (2.1%); heap flat at 74,504 B across an 8× corpus |
| 4 incremental commit | the second commit deleted the first generation | one document onto a 7.2 MB corpus writes 245 B, previous generation byte-identical |

**The real corpus, at the end.** The evaluation index was rebuilt at format v2 and
`make eval` re-run. All five milestone 4 arms reproduce to four decimals, both
binding deltas keep their confidence intervals, and the largest per-query moves
are unchanged. `Open` went from 979 ms to 54 ms. Recorded in
[EVAL.md](../EVAL.md) section 8.

## Review follow-up — a check the walk was providing

Found by reviewing this branch after CI went green, not by a plan task.

`segment.lookup` took the offset out of the terms index and handed it to a
`segReader` without bounding it. `segment.doc` bounds the offset it takes from
`docoff` on the line it uses it; `lookup` did not, and the asymmetry is what the
review noticed. The eager decoder has the check — `decodePostings` refuses a
term recorded anywhere but where its sequential walk sits — and `decodeTermIndex`
could not inherit it, because not walking is what makes `Open` lazy.

**RED** — commit `87a9901`.

```text
$ go test ./pkg/engine/ -run TestALyingTermOffsetIsNeverFollowed
panic: runtime error: slice bounds out of range [-6:]

engine.(*segReader).uvarint      segment.go:488
engine.decodeTermPostings        segment.go:950
engine.(*segment).lookup         segments.go:187
engine.(*Index).lookupAt         index.go:116
engine.(*Index).Lookup           index.go:371
```

Runtime RED, caused by the missing bound and nothing else: the test compiles,
runs, and reaches the panic through `Open` and `Lookup` on a directory whose
every checksum verifies. Reaching it needs exactly that — a doctored terms
section framed and checksummed the way the real writer frames it — which is why
neither fuzzer found it. CRC32C is an integrity code, not a signature.

**GREEN** — commit `1ce8a3a`.

```text
$ go test ./pkg/engine/ -run TestALyingTermOffsetIsNeverFollowed -v
--- PASS: TestALyingTermOffsetIsNeverFollowed/before_the_frame_header
--- PASS: TestALyingTermOffsetIsNeverFollowed/past_the_payload
ok      github.com/skyoo2003/weft/pkg/engine    6.113s

$ make all && make arch && make deps && make spdx
OK   # every gate, plus golangci-lint 0 issues
```

Three conditions on one line, the shape `doc` already used. `nil` rather than an
error, because `Lookup` has none to return — the D-006 trade, applied to the
sibling that had been answering with a panic.

## Test specification

| # | What is guaranteed | Test | Type | Result | Evidence |
| --- | --- | --- | --- | --- | --- |
| 1 | A version 1 segment is refused with `ErrBadVersion`, never migrated or misread | `formatv2_test.go:TestVersionOneIsRefused` | unit | PASS | `go test ./pkg/engine/` |
| 2 | Every `docoff` entry is the absolute file offset where that DocID's record begins, and decoding from there — with nothing before it read — yields that document | `formatv2_test.go:TestDocOffsetsLandOnRecordStarts` | unit | PASS | same |
| 3 | An out-of-range DocID reports false rather than indexing past the table | `formatv2_test.go:TestDocOffsetsLandOnRecordStarts` | unit | PASS | same |
| 4 | The `keys` section is strictly ascending and agrees with the DocIDs `docs` assigned; `lookup` finds present keys and reports absent ones without error | `formatv2_test.go:TestKeysSectionIsSortedAndAgreesWithDocs` | unit | PASS | same |
| 5 | The offset table is fixed width, so entry *i* is reachable by arithmetic — a uvarint table would pass #2 and #4 while leaving `Doc(id)` O(id) | `formatv2_test.go:TestDocOffsetTableIsFixedWidth` | unit | PASS | same |
| 6 | An overlong encoding of the current version is `ErrCorrupt` — two byte strings may not mean one index | `segment_test.go:TestOverlongVersionEncodingIsRefused` | unit | PASS | same |
| 7 | A future version is `ErrBadVersion`, not misread | `segment_test.go:TestOtherVersionsAreRefusedNotMisread` | unit | PASS | same |
| 8 | A `docoff` or `keys` section disagreeing with `docs` is refused rather than answering wrongly later | `persist.go:scrubDocs` and `seek.go:verifyKeyTable`, run by every `Scrub` | unit | PASS | `go test ./...` |
| 9 | Neither the frame parser nor the decoders panic on arbitrary bytes, the new section kinds included | `segment_test.go:FuzzParseSection`, `FuzzSegmentDecoding` | fuzz | PASS | 6.1M execs |
| 10 | The storage change costs the scorers and the fuser nothing | `git diff --stat main -- pkg/scorer pkg/fusion` | architecture | PASS | empty output |
| 11 | Any byte flip in `docs`, `postings` or `terms` is caught **with the frame checksum repaired** — the state every lazy read is in | `formatv2_test.go:TestEveryByteFlipIsCaughtWithoutTheFrameCRC` | unit | PASS | same |
| 12 | A healthy record decoded under another document's id is refused, so a damaged offset table cannot produce a plausible wrong answer | `formatv2_test.go:TestARecordDecodedUnderTheWrongIDIsRefused` | unit | PASS | same |
| 13 | Unsorted terms are refused **for being unsorted**, asserted on the reason rather than the sentinel | `segment_test.go:TestUnsortedTermsAreRefused` | unit | PASS | same |
| 14 | The 20-case lying-file matrix still trips the rule each case names, not the new checksum | `segment_test.go:TestLyingFilesAreRefused` | unit | PASS | same |
| 15 | A commit allocates a fraction of the segment it writes, not a multiple of it | `segment_test.go:TestCommitDoesNotBufferTheSegment` | unit | PASS | 695,536 B for 7,223,605 B |
| 16 | `Open` does not compute a section's whole-file checksum — the read lazy loading exists to remove | `lazy_test.go:TestOpenSkipsTheFrameChecksum` | unit | PASS | `go test ./pkg/engine/` |
| 17 | `Scrub` catches everything `Open` stopped catching, including every byte flip and every truncation of every file | `lazy_test.go:TestScrubCatchesWhatOpenNoLongerDoes`, `segment_test.go:TestEveryByteFlipIsCaught`, `TestEveryTruncationIsCaught` | unit | PASS | same |
| 18 | `Open` costs the vocabulary, not the corpus | `lazy_test.go:TestOpenDoesNotDecodeTheCorpus` | unit | PASS | 154,696 B for 7,223,605 B |
| 19 | A lazily opened index answers every read method exactly as the index that was committed | `lazy_test.go:TestLazyAndEagerAgreeOnEveryReadAPI`, `segment_test.go:TestSegmentRoundTrip`, `restore_test.go` | unit | PASS | same |
| 20 | Eight times the corpus does not mean more Go heap | `lazy_test.go:TestHeapDoesNotScaleWithTheCorpus` | unit | PASS | 74,504 B at 250 docs, 74,504 B at 2,000 |
| 21 | A damaged record is never served: not a wrong document, not a panic, neighbours intact, and `Scrub` names it | `lazy_test.go:TestADamagedRecordIsNeverServed` | unit | PASS | same |
| 22 | `Close` releases the mappings and leaves the index answering as empty rather than dangling; twice is a no-op | `lazy_test.go:TestCloseReleasesTheSegments` | unit | PASS | same |
| 23 | A commit writes what was added, not the corpus, and leaves earlier generations byte-identical | `lazy_test.go:TestCommitAfterOneAddWritesOneDocument` | unit | PASS | 245 B onto 7.2 MB |
| 24 | Generations accumulate, and a segment the manifest does not name is swept | `persist_test.go:TestSuccessiveCommitsAccumulateGenerations`, `TestUnmanifestedSegmentIsInvisible` | unit | PASS | same |
| 25 | `Merge` bounds the segment count | `lazy_test.go:TestMergeBoundsTheSegmentCount` | unit | PASS | same |
| 26 | A merge moves no document id, no posting list and no statistic | `lazy_test.go:TestMergeDoesNotMoveRankings` | unit | PASS | same |
| 27 | The same corpus produces the same bytes, committed or merged | `lazy_test.go:TestCommitIsByteDeterministic`, `TestMergeIsByteDeterministic` | unit | PASS | same |
| 28 | The milestone 4 evaluation reproduces on the new format | `weft-eval build` then `make eval` | integration | PASS | five arms to four decimals, both deltas with intervals |
| 29 | Commit atomicity, symlink refusal, generation bounds and foreign-entry refusal all survive the rewrite | `persist_test.go` (20 tests) | unit | PASS | `go test ./pkg/engine/` |
| 30 | A term offset the terms index does not justify is never followed — `Lookup` reports absence where it used to panic, on a directory whose every checksum verifies | `lazy_test.go:TestALyingTermOffsetIsNeverFollowed` | unit | PASS | same |

## Merge evidence

Checkpoint commits on `m3-scale`, oldest first:

| Commit | Stage | What it proves |
| --- | --- | --- |
| `70dd1f5` | RED | The four format v2 tests fail to compile, each on a symbol they newly reference |
| `89846d4` | GREEN | Same tests pass; `make all`/`arch`/`deps` green; 5.4M fuzz execs no panic; `pkg/scorer` and `pkg/fusion` diff 0 lines |
| `6108a01` | docs | This file |
| `eab4750` | RED | Five payload positions survive a flip once the frame checksum is repaired; a record decodes under the wrong id with no error |
| `b431d1e` | GREEN | Those positions all caught, wrong-id read refused; `make all`/`arch`/`deps` green; 6.1M fuzz execs no panic; scorer/fusion diff still 0 |
| `2924844` | docs | Task 1d written up |
| `28e201c` | RED | `Commit` allocates 585% of the segment it writes |
| `d5f6c79` | GREEN | 9.6%; the two allocations that dominated were an escaping varint scratch and a string conversion, neither of them the buffer being removed |
| `652b4c5` | RED | `Scrub` undefined; `Open` demonstrably computes every section's whole-file checksum |
| `0c18df8` | GREEN | Verification split three ways; `GOOS=windows` builds; the two exhaustive sweeps moved to `Scrub` |
| `824c954` | RED | `Index.Close` undefined; `Open` rebuilds the corpus |
| `3ef53a1` | GREEN | `Open` allocates 2.1% of the segment; six read methods unchanged; API grew by two names |
| `d599fd8` | RED | The second commit deletes the first generation |
| `4f9b6b1` | GREEN | 245 bytes for one document; earlier generations byte-identical; two callers fixed |
| `a6246c9` | RED | `Index.Merge` and `maxSegments` undefined |
| `20915a6` | GREEN | Count bounded, rankings unmoved, bytes deterministic |
| `d02869a` | GREEN | Heap flat at 74,504 B across an 8× corpus; commit determinism |
| `4de2e8f` | docs | FINDINGS verdict, FORMAT v2, D-006/D-007, EVAL section 8 |
| `87a9901` | RED | `Index.Lookup` panics on a doctored terms offset, reached through `Open` with every checksum verifying |
| `1ce8a3a` | GREEN | The offset is bounded where it is used; both cases report absence; every gate green |

If these are squashed, this table and the two blocks above are the surviving
record.
