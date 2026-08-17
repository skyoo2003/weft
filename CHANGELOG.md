# Changelog

What a caller has to change. Not what changed — `git log -p` already says that, and each tag's release notes are generated from the pull requests that went into it.

Three things can force work on your side, and each is decided by a file rather than by this document:

| What | Where it is decided | How you find out |
| --- | --- | --- |
| The exported API of any package under `pkg/` | `pkg/engine/testdata/engine_api.txt` for `engine`, `public_api.txt` for every other package there | those files' diff between the two tags. Usually your build stops compiling as well, but not always — an exported constant that changes value re-ranks your results and compiles cleanly, so the diff is the signal and the compiler is only a bonus |
| The on-disk format | `formatVersion` in `pkg/engine/segment.go` | `Open` returns `ErrBadVersion` |
| The minimum Go version | the `go` line in `go.mod` | `GOTOOLCHAIN=auto`, the default, downloads that version and builds; `GOTOOLCHAIN=local` refuses |

Those three have kinds of their own below. A release that moved none of them and has nothing else worth announcing gets no entry at all: absence is the claim.

**The module version and the format version are independent.** `v0.2.0` does not imply format 2, and a format bump does not force a major version, because weft is v0.x and its API can break inside a minor either way. They count two different things: one an API you compile against, one bytes already on your disk.

Entries are written with [changie](https://changie.dev) as the change is made, not reconstructed at release time. `make changelog-new` adds one.

## Unreleased

### Exported API

- Baseline. The surface at this tag is whatever pkg/engine/testdata/engine_api.txt and public_api.txt record at the tagged commit; every later release states its diff against them rather than restating the whole surface. ([#9](https://github.com/skyoo2003/weft/issues/9))
- Three additions, no changes: Scrub(dir) verifies every byte of a committed index, which Open no longer does; Index.Close releases the mappings an opened index holds; Index.Merge collapses the oldest segments once the count passes eight. The six read methods a scorer calls are untouched, which is the milestone 3 claim. ([#9](https://github.com/skyoo2003/weft/issues/9))
- engine.Index.Nearest(v []float32, k int) []DocID returns the DocIDs worth scoring exactly for a query vector, at least k of them when the index holds that many vectors. It computes no score: the engine knows which documents are geometrically plausible, and the metric stays with the caller (docs/DECISIONS.md D-008). The result is ascending, free of repeats, and wider than the answer — a segment with no partition offers every id it holds, documents carrying no vector included, so a caller still skips what it cannot score. The one thing left out is a segment holding no vectors at all: every candidate it could offer is one the caller would skip, so offering them would only buy a decode of the segment. It is not a promise that the exact top k are among them; docs/EVAL.md section 5.14 publishes the measured recall against a brute-force scan. ([#11](https://github.com/skyoo2003/weft/issues/11))

### On-disk format

- Version 1, described in docs/FORMAT.md. Nothing to migrate from — this is the first tag. ([#9](https://github.com/skyoo2003/weft/issues/9))
- Version 2. Version 1 indexes are refused with ErrBadVersion and are not migrated — weft has no users and the only v1 directory in existence is rebuildable, which is an argument available exactly once (docs/DECISIONS.md D-007). New sections docoff and keys let a DocID or a Key reach its document without decoding the documents in front of it; per-unit checksums let one record, block or entry be verified without reading the whole file; the manifest's segment entries now carry (name, base, count). ([#9](https://github.com/skyoo2003/weft/issues/9))
- Version 3. One new section, ivf, holding an IVF-flat partition of a segment's vectors: spherical k-means centroids and the segment-local DocIDs assigned to each, each inverted list sealed with a CRC-32C seeded with its own list number. Version 2 indexes are read without conversion — a v2 segment reports no partition and answers with every id it holds, which is the same fallback a pending segment and a segment below 16,384 documents already need, so it costs a branch that had to exist anyway. Index.Merge upgrades a v2 generation to v3 as a side effect of the maintenance an index already performs. Version 1 is still refused. This discharges the obligation FORMAT.md section 7.6 placed on a version 3 (docs/DECISIONS.md D-008). ([#11](https://github.com/skyoo2003/weft/issues/11))

### Minimum Go

- 1.26. This is the floor, not a tested ceiling — newer is untested rather than unsupported, and the gate pins this one line rather than a matrix. ([#9](https://github.com/skyoo2003/weft/issues/9))

### Added

- `weft-eval recall` and `make recall` measure what nDCG cannot see about an approximate vector index: overlap with a brute-force scan, candidates scored per query, latency both ways, and the working set a query actually reaches in bytes and in distinct pages. A partition that loses half the true neighbours holds nDCG steady when the neighbours it lost were unjudged, which on 50 queries against 171,332 documents is most of them. ([#11](https://github.com/skyoo2003/weft/issues/11))

### Changed

- Open maps segments instead of decoding them, so opening an index costs the vocabulary rather than the corpus — 54 ms against 979 ms on a 171,332-document index. Commit writes only what was added since the last one; on a 7.2 MB corpus a commit after one Add writes 245 bytes, and previous generations are left untouched. Whole-file verification moved from Open into Scrub: damage inside a unit is still refused when that unit is read, and damage in bytes no decoder reaches now needs Scrub to find. ([#9](https://github.com/skyoo2003/weft/issues/9))
- scorer/vector no longer scans the whole corpus. Its loop now reads engine.Index.Nearest, which on the evaluation corpus scores 30,549 documents of 171,332 per query and returns a query 4.6 times faster. The consequence a caller has to know about is that vector results are now approximate: recall@10 against an exact scan is 0.992 and nDCG@10 for the text+vector arm moved 0.6233 to 0.6211. Nothing about the scorer's contract changed — zero norms, non-finite queries, ErrDimMismatch and context polling all behave exactly as before, and all twelve of its existing tests pass unmodified. A segment with no partition is still scored exactly. ([#11](https://github.com/skyoo2003/weft/issues/11))

### Security

- Commit now creates its index directory and segment directories with mode 0o700 rather than 0o755. The corpus is the caller's data and nothing weft does needs another user on the machine to read it — including the caller's own group, which 0o750 would let read the 0o644 segment files inside. ([#9](https://github.com/skyoo2003/weft/issues/9))
