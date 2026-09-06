# On-disk format, version 5

`pkg/engine/segment.go`, `seek.go`, `ivf.go` and `persist.go` are what the bytes actually obey. Where this document and those files disagree, the document is wrong.

A format is the one thing in weft that cannot be quietly rewritten. Code is replaceable; a file on a user's disk demands a migration.

## Version history

| Version | Milestone | Change |
| --- | --- | --- |
| 1 | 2 | the first format. **Refused, not migrated** |
| 2 | 3a | `docoff` and `keys` — a `DocID` or a `Key` can reach its document without decoding the corpus |
| 3 | 3b | appended the `ivf` section |
| 4 | 11 | added `dead-<gen>` at the index root |
| 5 | 18 | appended named fields to each document record |

**Versions 2 through 5 are all read.** `Open` reports `ErrBadVersion` for v1.

That refusal is not technical. weft has no users, and the only v1 directory that existed was rebuildable from its sources. The argument rests on a user count and is therefore available exactly once — [D-007](DECISIONS.md) records it, and §7 says what it obliged version 3 to bring instead.

What v1 could not express: its `docs` section was a bare run of variable-length records and its key map was derived by reading all of them, so neither a `DocID` nor a `Key` could reach its document without decoding every document in front of it. No arrangement of a lazy reader fixes that.

### What version 3 brought is the reader, not a converter

v3 is v2 plus the `ivf` section. The fallback a segment without a partition needs — *every id is a candidate* — is the same one a pending segment and a segment below 16,384 documents already need. So reading v2 costs a branch that had to exist anyway.

`Index.Merge` is the only thing that upgrades a segment: it rewrites the run it collapses with the current writer, so no migration command and no rebuilt directory is needed.

What it is not is automatic. `Merge` does nothing below nine segments, weft never calls it for the caller, and past the ceiling it rewrites only the oldest run. A v2 index of one to eight segments stays v2 for as long as nobody adds to it, and answers vector queries with every id as a candidate — exact and slow, which is the whole reason the reader was affordable.

---

## 1. Directory layout

```text
dir/
  MANIFEST              the only entry point; one atomic rename publishes a commit
  dead-000007           the tombstone set, whole                  ← v4
  seg-000007/           one generation, immutable once the manifest names it
    meta                collection statistics — the BM25 snapshot
    docs                every document, in DocID order
    postings            the term dictionary, block-structured
    terms               term → absolute offset into postings
    docoff              DocID → (offset into docs, token count)   ← v2
    keys                sorted Key → DocID                        ← v2
    ivf                 centroids + inverted lists                ← v3
```

**The version decides the section list.** A v2 segment has six files; v3 and later have seven, and every frame inside one segment must declare the same version.

A v3 segment missing `ivf` is damage, not an older segment. A v2 segment with an `ivf` file beside it has a file nothing names. `meta` is opened first for exactly this reason: its frame is what says which list applies.

**Version 4 added no section to a segment.** `dead-<gen>` sits beside `MANIFEST` rather than inside a generation, so a v3 and a v4 segment are identical in shape. The reason it is at the root: **a commit which only deletes documents publishes no segment**, so there would be nowhere to put it.

**Version 5 added no section and no file.** It appends a field block to each `docs` record, after the timestamp and before the record's own checksum. The section list is unchanged, and a v4 record is a v5 record with the count absent. That is the cheapest shape §7 knows for a *record* and the most expensive one it knows overall — §7 argues that a section would have been cheaper still.

### How a commit shapes the directory

A commit writes **one segment holding what was added since the last commit**, and leaves previous generations untouched. A segment stores ids local to itself, counting from zero, and the manifest says where it sits in the index — so a segment's bytes do not depend on what was committed before it, which is what lets an old generation survive a new one byte for byte.

`Index.Merge` collapses the oldest run of segments into one when the count passes eight. Merging *adjacent* segments is concatenation: every document keeps the id it had. That is not a convenience — `engine.TopK` breaks ties on `DocID`, so a merge that moved ids would move rankings.

The manifest storing a *list* is version 1's doing, from [D-003](DECISIONS.md). Incremental commit extends its contents and not its shape, which is the one thing about v1 that survived contact with milestone 3.

**A segment directory the manifest does not name does not exist.** Nothing reads it, and `Commit` deletes it on its way past. `Open` leaves it alone: "unnamed" is also true of the segment a commit still in flight has written but not yet published, so a reader that swept would delete a live writer's work.

## 2. File frame

Every file, the manifest included, wears the same frame:

| Field | Bytes | Notes |
| --- | --- | --- |
| magic | 4 | `weft` |
| format version | uvarint | `5` when written, `2` through `5` when read; one byte until version 128 |
| kind | 1 | `1` meta, `2` docs, `3` postings, `4` terms, `5` manifest, `6` docoff, `7` keys, `8` ivf |
| payload | — | section-specific, §4 |
| checksum | 4 | CRC-32 Castagnoli, little-endian, over everything above |

`kind` exists because a checksum tells intact bytes from damaged ones and nothing more. Without it, a healthy `docs` file copied over `meta` would be parsed rather than refused.

Readers verify in this order, and the order is deliberate:

1. **magic** — a file that was never weft's reads as corrupt, not as a version problem.
2. **checksum** — so a flipped bit in the version byte cannot masquerade as a version mismatch.
3. **version** — checked only once the bytes are known intact.
4. **kind** — catches an intact file standing at the wrong path.

**`Open` skips step 2, and `Scrub` is where it went.** The frame checksum covers every byte of a file, so computing it costs a full read — the cost lazy loading exists to remove.

One consequence follows and is not hidden: on the `Open` path a damaged version byte reports `ErrBadVersion` rather than `ErrCorrupt`, because nothing has told the reader those bytes are damaged. `Scrub` runs all four steps and calls it corruption.

`segHeaderLen` is 6 (4 + 1 + 1). The terms index stores absolute file offsets, so this constant is part of the format, not an implementation convenience.

### Unit checksums

Each `docs` record, each `postings` block and each `terms` entry carries its own CRC-32C, four bytes, immediately after it. A reader that touches one unit verifies one unit, which is what makes lazy reading and integrity compatible.

Every unit checksum is **seeded** with what names the unit — a `DocID` for a record, its own absolute file offset for a block, its index for a terms entry — folded in as eight little-endian bytes before the unit's content.

The seed is the part that matters. A document record does not carry its `DocID`; its position did, and `docoff` is what turns an id into that position. Trust the table and one damaged entry yields a healthy record decoded under someone else's id — a plausible wrong answer rather than an error. Binding the id in makes the record prove which document it is.

The frame checksum still covers the unit checksums, so `Scrub` catches damage anywhere, including in the checksums themselves.

## 3. Primitives

- **uvarint / varint** — `encoding/binary`. Unsigned for counts, lengths, ids and frequencies; signed for Unix seconds, which are negative before 1970.
- **string** — uvarint byte length, then the raw bytes. UTF-8 is never assumed; keys and text round-trip as bytes.
- **float32** — 4 bytes, little-endian IEEE-754 bit pattern.

## 4. Sections

### MANIFEST

```text
generation      uvarint     strictly increasing; a write names its directory from it
segment count   uvarint
  segment name  string      e.g. "seg-000007"
  base          uvarint     the first DocID this segment owns
  count         uvarint     how many documents it holds
tombstones      uvarint     how many ids dead-<gen> holds             ← v4
```

**The bases must tile `[0, total)` contiguously and in ascending order.** A list that did not would give two segments overlapping ids, and a reader would answer with whichever it walked into first — a wrong document, not an error. Checked while the list is read rather than afterwards.

**No segment may be named for the next generation, and none past the current one.**

The first is what makes a write safe: each write clears `seg-<gen+1>` before using it, so a manifest already naming it would aim that removal at live data.

The second replaces a narrower rule versions 1 through 3 could state — *some* segment is named for the current generation — which v4 cannot keep, because a commit that only deletes documents publishes a generation and no segment, republishing the list it inherited. What stands in its place is stronger where it matters: every name is one some commit produced, and none is past the generation claiming to have written it, so a doctored manifest cannot smuggle in a segment named for a generation that has not happened.

Segment names come off disk and are validated, not trusted: a name must start with `seg-`, contain no path separator, and equal its own basename. A manifest naming `seg-../../etc` is refused rather than followed.

### meta

```text
document count  uvarint     ≤ 2³²−1, the DocID ceiling
total length    uvarint     Σ document token counts
vector width    uvarint     0 when no document carries a vector
live count      uvarint     ≤ document count; how many keys are indexed  ← v4
```

The first three are the collection statistics BM25 reads, written under the same read lock as the documents, so a commit is a point-in-time snapshot with its statistics intact ([FINDINGS §4.4](FINDINGS.md)). `Open` cross-checks all three against the `docs` file.

The fourth replaces a cross-check deletion took away. Every document is written, tombstones included — a `DocID` is a position in `docs`, so skipping one would renumber everything behind it — but only live ones get a `keys` entry, so "the keys table indexes as many keys as meta counts documents" stopped being true.

It is the count at *write* time, not a claim about now: documents are deleted after their segment is sealed and nothing rewrites a sealed segment. A v3 segment has no such field and every document it held was live, so it reads as the document count.

### docs

```text
document count  uvarint
per document:
  key           string      unique, non-empty
  text          string
  token count   uvarint     stored, not recomputed
  vector width  uvarint     0, or the corpus width
  vector        float32 × width
  link count    uvarint
  link          string × count
  time seconds  varint      Unix seconds
  time nanos    uvarint     0 … 999,999,999
  field count   uvarint     0 when the document has no fields          ← v5
  per field:                                                          ← v5
    name        string      non-empty, no NUL, unique in this document
    text        string
```

Seven decisions here are load-bearing.

**DocID is positional.** A document's index in this file *is* its DocID, so the id is never written and cannot contradict itself.

**Token count is stored, not recomputed** from the text at load. Recomputing would let a future change of tokenizer silently disagree with the postings the segment already holds.

Milestone 13 made that change of tokenizer possible, and the predicted disagreement is now **caught rather than committed**: `Open` recomputes one live document's tokens against this number and refuses the directory with `ErrTokenizerMismatch` when they differ. The ban above is on recomputing in order to *use* the answer, and it stands — no reader anywhere derives a length from text. This is one recomputation, once, in order to *compare*. Its four ceilings are in §8.

**Links are keys, not DocIDs** ([FINDINGS §4.2](FINDINGS.md)). Lazy resolution is what makes forward references and dangling edges free, and milestone 4's evaluation joins an external citation graph by key.

**A field is a term space, not a section.** Every field's text is split by the same tokenizer and its terms go into the same `postings` under `name + NUL + token`, which is what `engine.FieldTerm` spells. So a query scoped to a field is an ordinary term lookup, nothing in the index learns what a field means, and adding fields cost no new section, no new table and no new rejection rule beyond the three on the names.

**A document's stored token count includes every field's tokens.** Forced rather than chosen: field terms are ordinary postings, and §5 already refuses a segment whose per-document frequencies do not sum to its stored length. What it costs is that there is no per-field length to normalize BM25 against — §8 prices it, and `scorer/text`'s field-scoped form answers it by not normalizing at all.

**Fields are a slice, not a map.** `Commit` is byte-deterministic and a map has no order, so two indexes holding the same documents would produce different segments.

**Time carries no presence flag.** The zero `time.Time`'s own Unix seconds decode back to a value that `IsZero` again, so "no timestamp" — which the recency scorer reads as "no opinion" — survives by arithmetic rather than by convention. The zone is dropped; a restored time is the same instant in UTC, and instants are all any scorer reads.

### postings

```text
term count      uvarint
per term (in the terms index's order):
  block count   uvarint
  per block:
    posting count uvarint    = 128 except in the last block
    maxDocID      uvarint    last DocID in this block
    maxTF         uvarint    highest term frequency in this block
    minDocLen     uvarint    shortest document in this block
    per posting:
      DocID       uvarint    absolute for each block's first posting, then a delta ≥ 1
      frequency   uvarint    ≥ 1, and ≤ the document's token count
```

**Delta chains stop at the block boundary.** Every block's first DocID is absolute, so a block can be decoded without its predecessors — the entire point of the metadata, since a skipper that had to decode every preceding block to learn where this one starts would be skipping nothing. It costs at most three bytes per block, and retrofitting it would cost a format migration.

Term strings are **not** in this file. They live in the terms index, and entries are located by the offsets recorded there.

`maxDocID`, `maxTF` and `minDocLen` are [D-001](DECISIONS.md): everything block-max WAND needs, written from the start because retrofitting them is a migration while writing them costs three varints per block. **No query reads them before milestone 5.** They are re-derived from each block's contents and compared on every `Open`, which is what stops an unread field from rotting.

Block size 128 is convention from the block-max WAND literature, not a measurement — marked `ponytail:` in the source.

### terms

```text
term count      uvarint
per term:
  term          string       strictly ascending
  offset        uvarint      absolute file offset of the term's postings entry
```

This is milestone 3's seek structure and no query uses it yet. It is nevertheless **load-bearing today**: the postings file holds no term strings, so a segment cannot be read without it. That is deliberate — an index nothing reads is an index that rots, and this one cannot.

### docoff (v2)

```text
document count  uvarint
  offset        uint64 LE   absolute file offset of the record in docs
  token count   uint64 LE   the same number the record itself carries
```

Fixed width, sixteen bytes an entry, and that is the whole point: entry *i* is at a computable position, so `Doc(id)` is arithmetic plus one record decode. A uvarint table would have to be walked from the front, which is the cost being removed.

Eight bytes for the offset rather than four, because a 656 MB `docs` file already sits one order of magnitude from a `uint32` ceiling and raising it later is a migration.

The token count rides along rather than being read out of the record, because BM25 asks for a length once per posting and reaching it through the record would make every posting cost a key, a text and a vector. The record carries the length too, and the two are compared — a copy nobody checks is what [D-001](DECISIONS.md) is about.

### keys (v2)

```text
key count       uvarint
  offset        uint64 LE      absolute file offset of the entry, in key order
  (reserved)    8 bytes        written zero; the table shares docoff's 16-byte width
  key           string         the entries themselves, ascending
  document id   uvarint        segment-local
```

The reserved half is padding, not a field: the table reuses `docoff`'s entry width and has no token count to put beside the offset. Eight bytes a document, kept because the width is on disk — narrowing it belongs to a version bump, not to a reader that would then disagree with every v2 index already written.

The offset table makes `Resolve` a binary search rather than a map rebuilt by reading every document. Ascending order is what the search rests on, so an unsorted table does not fail — it answers wrongly — and the decoder checks it.

Offsets are computed forward rather than patched back in: an entry's encoded length is known from the key and the id, and the table's own size from the count. A writer that had to seek backwards could not stream, and merge cannot buffer a corpus.

### ivf (v3)

```text
list count      uvarint     nlist; 0 means no partition, and then dim is 0 too
centroid width  uvarint     dim; must equal meta's vector width
  centroid      float32 × dim, LE, L2-normalized      × nlist
  member count  uvarint                                × nlist
  list offset   uint64 LE   absolute file offset       × nlist
per list, in list order:
  DocID         uvarint     absolute for the first, then a delta ≥ 1
  checksum      4           CRC-32C seeded with the list's own number
```

An IVF-flat partition of the segment's vectors: spherical k-means centroids, and the segment-local DocIDs assigned to each. `Index.Nearest` ranks the centroids by inner product with a query, reads the best `nprobe` lists, and returns their members as the documents worth scoring exactly. **No score is stored and none is computed here** — the metric belongs to the caller, which is [D-008](DECISIONS.md).

Four decisions here are load-bearing.

**The section is always written**, with `nlist = 0` when the segment holds fewer than 16,384 documents or no vectors. A section list that varied with the corpus would make the rejection table say "may or may not be here", and every reader would have to know which.

`nlist = 0` means *every id in the segment is a candidate*. One case is different rather than a fourth spelling of this: a segment whose `meta` claims no vector width at all contributes **no** candidates, because a scorer skips a document with no vector before it compares widths — so those ids can produce neither a score nor a width-mismatch error, only a decode of every record in the segment.

**Spherical, not plain L2.** Vectors are normalized before training and the assignment maximizes an inner product. Ranking is by cosine, and partitioning by raw L2 would gather the documents with the largest norms into one list regardless of direction, losing exactly the neighbours a cosine query wants.

**The offset table is fixed width**, for the reason `docoff`'s is: a query probes a few lists out of up to a thousand, and reaching list *j* through a uvarint table would mean decoding the *j−1* lists in front of it. At eight bytes an entry it is at most eight kilobytes. Offsets are computed forward, so the writer still streams.

**Each list's checksum is seeded with its own number.** A list carries no copy of which centroid it belongs to, so a reader following a damaged offset would decode a *healthy* list under the wrong centroid. That is not an error, it is a plausible wrong candidate set — and a wrong candidate set is indistinguishable from ordinary recall loss, which is the one failure an approximate index must not be allowed to hide.

**Parameters.** `nlist` is `min(⌈√count⌉, 1024)`, from the document count rather than the vector count, then clamped to the number of vectors training actually sampled, because k-means with more centroids than points has no fixed point.

`nprobe` is a constant 64 and is not configurable. `Nearest` raises it on its own when a query would otherwise return fewer than *k* candidates, so "at least k" is a contract rather than a tuning exercise. `nprobe` is the reader's policy and is not stored, so changing it needs no rebuild. `nlist` is the writer's: it is the section's first field and everything after is sized by it, so changing how it is derived rewrites every segment.

**Damage in a list costs speed, not availability.** A list that fails its own checksum makes the whole segment behave as though it had no partition: every id becomes a candidate, the answer becomes exact, and the query becomes slow. That is [D-006](DECISIONS.md)'s rule where absence is the safest answer, and `Scrub` is what names the damage.

### dead-&lt;gen&gt; (v4)

```text
tombstone count uvarint     must equal the count MANIFEST carries
  id delta      uvarint     first absolute, then strictly positive gaps
```

The one file that is not part of a segment. It holds the whole tombstone set of the index, ascending, and it is republished under the new generation by every `Commit` and every `Merge` — so the live generation's file is the entire set and a reader needs exactly one. `prune` removes every other.

Deltas rather than fixed-width ids because nothing seeks into this section: every reader of it wants all of it, unlike `docoff` and `keys`. A corpus with a large fraction deleted has small gaps, so the file is about a byte per tombstone.

**It carries no per-entry checksum, and its frame checksum is verified eagerly.** That is the opposite of the choice made inside a segment, and the size is why: this section is the size of the tombstone set, not of the corpus. What it buys is that the one structure standing between a query and a deleted document cannot be quietly wrong.

**Absence is corruption, not an empty set.** `MANIFEST` carries the count, so a file lost to a bad prune or a partial copy is refused rather than read as "nothing was deleted" — which would hand back every document it named. A commit writes the file even when the set is empty, so that the one state which must not be ambiguous is not the same state as a file that went missing.

**Nothing here reclaims anything.** A tombstoned document keeps its record, its `docs` bytes and its postings, and every `Merge` copies them forward. Emptying the slot would mean renumbering, and ids are what `TopK` breaks ties on, what keeps posting lists ascending, and what lets a merge be a concatenation. §8 prices it.

## 5. What a reader rejects

Bytes from disk are a trust boundary. Every failure is an error wrapping `ErrCorrupt` or `ErrBadVersion`. None is a panic, and none is an index that looks plausible but breaks an invariant a scorer relies on.

### Frame and layout

| Rejected | Why it matters |
| --- | --- |
| Checksum mismatch, truncation, trailing bytes | Damage, or a file this encoder did not write |
| Wrong magic, wrong kind, unknown version | Foreign file, misplaced file, future format |
| The version written in more than one byte | `binary.Uvarint` decodes `0x82 0x00` as 2 too. The header would be seven bytes while `segHeaderLen`, and every offset recorded against it, still says six |
| A section file the manifest names but that is not on disk | Reported as `ErrCorrupt` and never as `fs.ErrNotExist`, so a caller's "nothing committed yet" branch cannot overwrite a damaged index |
| Two sections of one segment declaring different format versions | Two versions are accepted, so "every frame says the same number" stopped being enforced by the version check. A segment whose `meta` says 3 and whose `docs` says 2 passes every frame check individually and describes nothing a writer produced |
| A segment directory or section file that resolves outside the index directory | Both readers work through an `os.Root`, so a symlink planted where a segment belongs is refused by the OS, not by a check a rename could race |
| An entry of the wrong kind at any path in the layout | A plain file or symlink where a segment directory belongs, a directory or symlink where a section file belongs, or either at `MANIFEST` itself. All report `ErrCorrupt` rather than raw `ENOTDIR`/`EISDIR` — including the entry point, whose raw `EISDIR` would otherwise be neither `ErrCorrupt` nor `fs.ErrNotExist`, leaving a caller with no branch to take. The kind is asked with `Lstat`, not `Stat`, because `Stat` follows the very link in question |

An entry of the right kind that is genuinely there and still will not open is the filesystem refusing us, not corruption, and reports as itself.

### Manifest

| Rejected | Why it matters |
| --- | --- |
| A generation whose segment is the one the next write would produce | `Commit` and `Merge` both clear a half-written segment directory before writing it; aimed at a live segment, that clears published data |
| A manifest naming no segment for its own generation | The generation counter and the directory would have stopped describing each other |
| Segment bases that do not tile `[0, total)` contiguously and in order | Two segments would own the same ids, and a reader would answer with whichever it walked into first |
| A `docoff` or `keys` count disagreeing with `meta` | The seek tables and the statistics BM25 trusts would be describing different corpora |
| A generation of `0` or `MaxUint64` | The counter advances one commit at a time from a first commit that publishes `1`. `0` describes a commit that never happened: accepting it would load a foreign layout, let the next `Commit` publish over it, then sweep away a `seg-000000` weft never wrote. `MaxUint64` is what `Commit`'s `generation + 1` wraps to zero on |

### Documents and postings

| Rejected | Why it matters |
| --- | --- |
| Empty or duplicate key | `Add` refuses both; a restored index must not hold what a live one cannot |
| A field name that is empty, holds a NUL, or repeats within one document | Each is a term space no lookup could reach, so a reader accepting one would answer queries about a field that does not exist |
| NaN or infinite vector component | `scorer/vector` skips re-checking documents because `ErrNonFiniteVector` promised this |
| Mixed vector widths | Mixed embedding models |
| meta disagreeing with docs | The BM25 snapshot must describe its own corpus |
| Postings not strictly ascending, or naming a document past the corpus | Block skipping and the `TopK` tiebreak both assume dense ascending DocIDs |
| Frequency of 0, or above what the document has left to account for | The bound is the document's *remaining* token budget, not `docLen`: bounding each posting on its own would let two terms each claim one occurrence in a one-token document, and BM25 would divide real frequencies by a length that never held them. Checking the budget first is also what stops the running sum from wrapping |
| A document's frequencies summed across all terms falling short of its stored length | Every token `Add` saw became exactly one posting increment, so the two must agree exactly |
| A DocID delta whose sum overflows uint64 | The wrapped id lands back inside the corpus and passes every later check while breaking ascending order |
| Block metadata contradicting block contents | The D-001 rot check |
| A non-final block that is not full | Block and posting counts would stop agreeing |
| A block whose first DocID does not exceed the previous block's last | The block continued a delta chain instead of starting absolute |
| Terms unsorted, or an offset not landing on its entry | Milestone 3 would inherit a broken seek structure |
| A unit whose checksum does not match, seeded with what names it | Covers damage no semantic rule can see — a flipped byte of document text is invisible to every other check — and covers a record reached through a damaged offset, which would otherwise decode healthily under someone else's id |

### The ivf section

| Rejected | Why it matters |
| --- | --- |
| A v3 segment with no `ivf` file; `nlist` above 1024; centroids not as wide as `meta`'s vectors, or of no width beside a non-zero `nlist`; a member count above the segment's document count, or the counts summing past it | The version fixes the section list, and the rest are states the writer cannot produce: one document belongs to at most one list, so the counts partition the segment. A width of zero is the one the width comparison alone misses, because a segment holding no vectors claims zero in `meta` too |
| Centroids whose count times their width would not fit in the bytes that follow | Checked by dividing the remaining payload, not by multiplying `nlist` by a width `meta` bounds only at `maxInt`: that product wraps a `uint64`, and a wrapped product is a check that passes and an allocation that panics |
| A non-finite centroid component | It would poison every comparison it takes part in, and the ranking sorts NaN last rather than refusing it — so the query would silently never probe that list |
| A centroid whose components are all zero | The same failure with a quieter face: it scores zero against every query, sorts below any list with a positive inner product, and its own documents stop being reachable. Zero exactly, not near-unit — a tolerance would be a constant with no measurement behind it |
| An `ivf` list not strictly ascending, naming a document past the segment, or failing its seeded checksum | Refused **by `Scrub`**. On the read path the same conditions make the segment answer as though it had no partition, because a slow exact answer beats an error (D-006) |

### Version 4's own rejections

All are cases where the alternative is a plausible wrong answer rather than a crash.

| Rejected | Why it matters |
| --- | --- |
| `dead-<gen>` missing from a v4 generation, whatever the count | Nothing names this file, so absence would read as "nothing was deleted" and every deleted document would come back. The **version** decides whether to read it, never the count — a zero count is the case where skipping it looks harmless and is not |
| its count disagreeing with `MANIFEST`'s | The two are written by one commit and read by different paths; a disagreement means one is not describing this index |
| a repeated id | Counted twice, the live document count is one too low for the life of the index |
| an id at or past the corpus size | Damage, or a tombstone file copied in from a larger index, where it hides whichever documents land on those ids here |
| `keys` indexing a number of entries other than meta's live count | The check that "one key, one document" replaced, once tombstones made the document count the wrong denominator |
| two *live* records carrying one Key | One live holder per key is what `Resolve` rests on. Two records carrying one key is legal and is what every update leaves behind; two *live* ones is a state neither `Add` nor `Update` could produce |
| a segment named past the generation claiming to have written it | A doctored manifest naming a generation that has not happened |

### What only `Scrub` checks

`Open` verifies the frame header, the manifest and `meta`, and each unit as it is read. Everything else above — the whole-file checksums, every document, every posting list, both seek tables in full — is `Scrub`'s.

The gap that leaves is real and worth stating: **a unit nothing ever reads is never verified**, so rot in a document no query reaches sits there until somebody runs `Scrub`. Milestone 2 got this free because `Open` read every byte; milestone 3 buys it explicitly.

### Two things nothing checks, stated plainly

**No reader verifies that every vector-bearing document appears in some `ivf` list.** A document missing from the partition is invisible to a vector query, and the cost of that is recall — the same currency an approximate index spends by design, and not separable from it by any rule available on either path. Buying the check would mean carrying one bit per document out of the `docs` walk, and the answer it would give is "this index recalls slightly less than it should", which `weft-eval recall` measures directly against a brute-force scan.

**No reader verifies that a term matches the text of the documents it names.** Nothing re-tokenizes `Text` to compare per-document term frequencies, so a doctored file can file a document's postings under a token its text does not contain, and text search will return it for that token.

That is accepted, for the same reason `docLen` is stored rather than recomputed: a segment records what *was* indexed, not what this build's tokenizer would index today, and a reader demanding the two agree would refuse every segment written before a tokenizer change. Term-to-text correspondence is not an invariant the scorers rest on, and buying it would cost a full re-tokenization of the corpus on every `Open`.

## 6. Atomicity and durability

`Commit` writes the segment's files, fsyncs each, fsyncs the segment directory, fsyncs the index directory so the segment directory's own entry is durable before anything names it, then renames a temporary manifest over `MANIFEST` and fsyncs the index directory again.

Syncing only the inside of the segment would leave the rename free to land first, and a manifest naming a directory whose entry never made it is the mixed state the rename exists to rule out.

**Guaranteed against process death, on POSIX.** The rename is atomic there, so a crash at any point leaves either the previous generation or the new one — never a mix, and never a partly visible segment. A segment written but not yet named by a manifest is indistinguishable from one that was never written.

The guarantee is the platform's, not weft's. Go's `os.Rename` contract does not promise atomic replacement everywhere it compiles — Windows in particular — and weft calls no platform-specific replacement primitive. Linux and macOS are what milestone 2 claims and what its tests run on.

**Best-effort against power loss.** Contents and directory entries are fsynced, but weft uses no platform-specific write barrier: no `F_FULLFSYNC` on macOS, no device cache flush. Directory syncing is unsupported on some systems, where the error is deliberately ignored. A filesystem that reorders aggressively can still lose a commit that `Commit` reported as done. Tests pin the process-crash guarantee; the power-loss case is documented, not tested.

`Commit` refuses to run against a corrupt manifest rather than guessing the next generation, because writing on top of a directory in an unknown state could orphan a commit the caller believes exists.

### A first commit has to establish that the directory is weft's

`Commit` deletes every `seg-*` entry it finds — its own target before writing it, the rest in the sweep afterwards — and a manifest is what proves those names are weft's own debris.

With no manifest there is nothing to say that `seg-000001` is debris rather than a caller's directory sitting under a name weft happens to reserve. `Commit` aimed at a home or documents directory would recursively delete data it never wrote. The same goes for `MANIFEST.tmp`.

A commit that crashed before its rename must still be recoverable, so the test is what such a commit leaves behind rather than mere absence:

- A `seg-*` entry may exist, but only as a real directory holding nothing but regular section files.
- `MANIFEST.tmp` may exist too.
- **Every one of those files has to carry weft's magic.** A reserved name and a file type are things a caller's own data can have by coincidence; four bytes of magic are not.
- A file the writer created but never filled satisfies that as well — it buffers until close, so its debris is empty or a prefix of the frame.

Anything else refuses before the commit mutates anything at all: a stray entry inside the segment, a *directory* named `meta` (which the recursive clearing would take everything beneath), a symlink at any of those names, or a file under one of them that weft did not write.

The magic is the ownership signal and the only one available here. The kind byte says which section a file is rather than whose it is, and a torn write may stop before reaching it. A frame intact enough to carry the magic but broken past it is weft's own debris, and the reader's checksum is what refuses it as an index.

### Concurrency

One writer at a time. `Commit` is safe alongside `Add` and queries — it takes the same read lock a query does — but not alongside another `Commit` on the same directory.

`Open` performs no deletions and no writes, so opening a directory while it is being committed to cannot damage it. A reader that loses the race against `Commit`'s sweep of the previous generation sees an error and can retry.

### Nothing weft opens leaves the index directory

`Commit` and `Open` both work through an `os.Root` on that directory, so every path below it is resolved by the OS with the guarantee that it stays beneath the root. A symlink standing where `seg-000007` belongs is refused rather than followed, and the refusal is not a check of weft's that a rename could race.

Two things follow, and one does not:

- **The manifest's name check and the root do different jobs.** `seg-000007` is a syntactically perfect name whether the entry standing there is a directory or a link into somebody else's index. The first question is about the manifest's bytes, the second about the filesystem.
- **The temp manifest is additionally created with `O_EXCL`**, because it sits at a predictable path. Without it, a symlink planted at that name would be written through — the difference between "can write in the index directory" and "can overwrite any file this process can reach".
- It is **not** a claim that the directory's contents are trustworthy. Somebody who can write there can plant a whole valid segment, and weft would read it — which is why every value in it is validated anyway.

## 7. Changing this format

**The rules.**

1. **Unknown versions are refused, never guessed.** "Probably compatible" is how a wrong index gets loaded silently.
2. **Any change to what the bytes mean bumps `formatVersion`** — a new field, a retyped field, a reordered field, a relaxed invariant.
3. **The frame does not change.** Magic, version and kind must stay where they are, or a future reader cannot even reach the version in order to reject it.
4. **Migration, when it comes, reads the old version and writes the new one.** There is no in-place upgrade path.

**What each version taught.**

**Multiple segments needed no bump, and lazy reading did.** The manifest already carried a list and the segment format is per-segment, so incremental commit and merge changed contents rather than shape — [D-003](DECISIONS.md) designed for that and it held. What forced version 2 was smaller and inside a single segment: `docs` was positional and the key map was derived, so neither a `DocID` nor a `Key` could reach its document without decoding the corpus.

**Version 2 skipped the converter, and no later version may.** v1 is refused rather than converted, on the single ground that weft had no users and the one v1 directory in existence was rebuildable. That is an argument about a user count, so it expires the moment somebody has an index they cannot rebuild ([D-007](DECISIONS.md)). A version 3 owed either a converter or a reader that understands both.

**Version 3 paid that debt with the reader, and it was nearly free.** v3 appends `ivf` and changes nothing else, and a segment without a partition has to be readable regardless — a pending segment has none, a segment below 16,384 documents has none, and a damaged partition is treated as none. So "v2 is a segment with no partition" reuses a branch that already existed, where a converter would have been a command, a rebuild, and a directory in two states while it ran.

The lesson: **append a section rather than changing one**, and the old version stays readable by construction. A version that retypes or reorders an existing field gets no such discount and owes the converter.

**Version 4 paid it the same way, and more cheaply still.** v4 appends a file at the index *root* and one uvarint to each of two existing sections, all at the end of their payloads. A v3 segment is not a v4 segment missing a section — it is the same seven files — and the two fields it lacks have answers that need no branch: no tombstone file means nothing was deleted, and a `meta` with no live count means every document it held was live.

So the lesson sharpens: **a root-level file costs less than a segment section**, because it does not enter the per-version section list at all. What v4 could not avoid is the version bump itself, and that is the *point* rather than the price — nothing names the new file, so a build that predates it would answer with every deleted document still sitting in the segments. Stamping `MANIFEST` with 4 makes such a build refuse the whole directory at the first frame it reads.

**Version 5 appended to a *record*, and that was the expensive kind of append.** A `docs` record is a **unit**: it carries its own checksum, seeded with its `DocID`, inside a section whose per-record offsets live in a second file. So one appended field moved three things at once — every record's checksum, every `docoff` entry behind it, and both files' frame checksums.

Reading an older version stayed free, because a v4 record is a v5 record with the count absent and the version already decides whether to look for it. What was not free is anything that has to *produce* older bytes: the test helper that downgrades a generation is a backwards walk over a trailing varint for versions 3 and 4, and a full re-encode of `docs` and `docoff` for version 5.

The alternative was a `fields` section with an offset table of its own — a new file, a new entry in the section list, a new fixed-width table and its own rejection rules. More code, and it would have left the record untouched.

**The rule this leaves for a version 6: append to a section, not to a record, unless the field belongs to the record's identity.** A field's text does belong to it, which is why this was still the right trade. A corpus-derived statistic would not.

## 8. Known limits

### Capacity and deletion

| Limit | Value | Where it goes |
| --- | --- | --- |
| Documents per index | 2³²−1 | `DocID` is uint32; `Add` refuses past it |
| Deletion reclaims nothing | A deleted document keeps its record, its bytes and its postings forever | Renumbering is the only alternative and ids are load-bearing; a full re-index is the only compaction ([FINDINGS milestone 11](FINDINGS.md)) |
| An update spends a DocID | The ceiling is 2³²−1 ids, not documents | A corpus updated hot enough exhausts ids before documents. Unmeasured |
| A corpus-walking scorer still walks tombstones | `Len` is the id bound, so `scorer/recency` visits deleted ids and skips them | The cost of keeping `Len` meaning what a scorer needs ([FINDINGS milestone 11 §2](FINDINGS.md)) |
| DocID namespacing | None — a DocID means nothing outside its index | Unresolved |

### What the index cannot express

| Limit | Value | Where it goes |
| --- | --- | --- |
| No term positions | A posting is `{DocID, Freq}` | So no phrase or proximity constraint can be decided from the index. A caller decides one from `Document.Text` instead, at a record decode per candidate. Storing positions widens the postings block encoding — a **format v6** at the earliest, and not planned ([ADOPTION §8](ADOPTION.md)) |
| No per-field length | A document's stored token count is `Text` plus every field, with no record of which was which | So BM25 has nothing to normalize a field match against. Normalizing by the whole document's length would divide a five-token title by a five-thousand-token body and rank by brevity, so `scorer/text`'s field-scoped form sets `B` to 0. What that gives up is separating two equally good field matches by field length. A per-field count is a **format v6** and is not bought before something measures that it is needed |
| A field's terms and `Text`'s do not mix | A term in `Text` and the same term in a field are two different terms | Deliberate — it is what makes a scoped query mean anything. A caller wanting a word findable both ways puts it in both places, which counts its tokens twice toward the document's length |
| The field separator is not checked on the token side | Field terms are `name + NUL + token`, and nothing verifies that a token holds no NUL | The default tokenizer cannot produce one, but a caller-supplied `Tokenizer` could, and such a token could collide with some field's term and share its posting list. Checking it is a byte scan of every token of every document |

### Vector search

| Limit | Value | Where it goes |
| --- | --- | --- |
| Vector search is approximate | recall@10 = 0.992 against a brute-force scan on the evaluation corpus | The screw is `nprobe`, and it is not exposed. [EVAL §5](EVAL.md) carries the curve |
| `Nearest` weakens under deletion | It promised at least k candidates when k vectors exist; tombstones are filtered after the segment widened its probe | Recall falls as the deleted fraction rises. The widening loop would have to know about tombstones |
| A vector query's working set | 210 MiB per query of a 626 MiB `docs` section | The partition cut the arithmetic 5.6× and the bytes 3.0×. Why the second is so much worse, and what would fix it: [FINDINGS milestone 3b](FINDINGS.md) |
| Build cost | +68 s on the 171,332-document evaluation corpus | Constant per commit and per merge, not per query. Below 16,384 documents it is not paid at all — the floor is `4·nprobe²`, the size at which a partition first narrows a query to half the segment |
| `nlist` ceiling | 1024 | The assignment pass is linear in it |

### The tokenizer guard

| Limit | Value | Where it goes |
| --- | --- | --- |
| The tokenizer's identity is not stored | Nothing on disk says which tokenizer split these documents | Deliberate. A Go function value has no stable name, so anything stored would be a caller-supplied label, and a caller who swaps tokenizers without editing the label makes the check confidently wrong. Bytes cannot lie about what split them ([D-023](DECISIONS.md)) |
| The check compares a count, not the terms | `Open` recomputes one live document's tokens and compares against the number `docoff` holds. Disagreement is `ErrTokenizerMismatch` | Catches the large failure — a bigram index opened with the default, 7 tokens against 2 — and **not** a tokenizer that preserves token count. A stemmer is exactly that shape: one token in, one token out, every term changed and no count changed |
| The check samples one document | The first live document whose stored token count is non-zero | Re-tokenizing the corpus would make `Open` cost the size of the index, which is the whole of what mapping it rather than loading it bought. A corpus with no text to judge is admitted — it answers nothing under every tokenizer |
| A non-deterministic tokenizer disagrees with itself | Both the postings and the check assume the same string yields the same terms | `Tokenizer`'s doc comment makes determinism the contract. Nothing enforces it, and violating it makes `Open` refuse the directory it just wrote |
