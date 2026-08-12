# On-disk format, version 1

`pkg/engine/segment.go` and `pkg/engine/persist.go` are what the bytes actually
obey. Where this document and those files disagree, the document is wrong.

Milestone 2 writes this format; milestone 3 extends it. It is written down
because a format is the one thing in weft that cannot be quietly rewritten — code
is replaceable, a file on a user's disk demands a migration.

---

## 1. Directory layout

```
dir/
  MANIFEST              the only entry point; one atomic rename publishes a commit
  seg-000007/           one generation, immutable once the manifest names it
    meta                collection statistics — the BM25 snapshot
    docs                every document, in DocID order
    postings            the term dictionary, block-structured
    terms               term → absolute offset into postings
```

Version 1 writes **exactly one segment per generation**, and each commit rewrites
the whole corpus. The manifest nevertheless stores a *list* of segments, so
milestone 3's incremental segments extend the contents of this format rather than
the format itself.

A segment directory the manifest does not name does not exist. Nothing reads it,
and `Commit` deletes it on its way past. `Open` leaves it alone: "unnamed" is also
true of the segment a commit still in flight has written but not yet published, so
a reader that swept would delete a live writer's work.

## 2. File frame

Every file, the manifest included, wears the same frame:

| Field | Bytes | Notes |
|---|---|---|
| magic | 4 | `weft` |
| format version | uvarint | `1`; one byte until version 128 |
| kind | 1 | see below |
| payload | — | section-specific, below |
| checksum | 4 | CRC-32 Castagnoli, little-endian, over everything above |

`kind` is `1` meta, `2` docs, `3` postings, `4` terms, `5` manifest. It exists
because a checksum tells intact bytes from damaged ones and nothing more: without
a kind byte, a healthy `docs` file copied over `meta` would be parsed rather than
refused.

Readers verify in this order, and the order is deliberate:

1. **magic** — a file that was never weft's reads as corrupt, not as a version problem.
2. **checksum** — so a flipped bit in the version byte cannot masquerade as a version mismatch.
3. **version** — checked only once the bytes are known intact.
4. **kind** — catches an intact file standing at the wrong path.

`segHeaderLen` is 6 (4 + 1 + 1). The terms index stores absolute file offsets, so
this constant is part of the format, not an implementation convenience.

## 3. Primitives

- **uvarint / varint** — `encoding/binary`. Unsigned for counts, lengths, ids and
  frequencies; signed for Unix seconds, which are negative before 1970.
- **string** — uvarint byte length, then the raw bytes. UTF-8 is never assumed;
  keys and text round-trip as bytes.
- **float32** — 4 bytes, little-endian IEEE-754 bit pattern.

## 4. Sections

### MANIFEST

```
generation      uvarint     strictly increasing; the segment directory is named from it
segment count   uvarint     always 1 in version 1
segment name    string ×n   e.g. "seg-000007"
```

Segment names come off disk and are therefore validated, not trusted: a name must
start with `seg-`, contain no path separator, and equal its own basename. A
manifest naming `seg-../../etc` is refused rather than followed.

### meta

```
document count  uvarint     ≤ 2³²−1, the DocID ceiling
total length    uvarint     Σ document token counts
vector width    uvarint     0 when no document carries a vector
```

These three are the collection statistics BM25 reads, written under the same read
lock as the documents, so a commit is a point-in-time snapshot with its statistics
intact ([FINDINGS §4.4](FINDINGS.md)). `Open` cross-checks all three against the
`docs` file and refuses a segment whose meta disagrees with its own documents.

### docs

```
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
```

Four decisions here are load-bearing:

- **DocID is positional.** A document's index in this file *is* its DocID, so the
  id is never written and cannot contradict itself.
- **Token count is stored, not recomputed** from the text at load. Recomputing
  would let a future change of tokenizer silently disagree with the postings the
  segment already holds.
- **Links are keys, not DocIDs** ([FINDINGS §4.2](FINDINGS.md)). Lazy resolution
  is what makes forward references and dangling edges free, and milestone 4's
  evaluation joins an external citation graph by key.
- **Time carries no presence flag.** The zero `time.Time`'s own Unix seconds
  decode back to a value that `IsZero` again, so "no timestamp" — which the
  recency scorer reads as "no opinion" — survives by arithmetic rather than by
  convention. The zone is dropped; a restored time is the same instant in UTC,
  and instants are all any scorer reads.

### postings

```
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

Delta chains **stop at the block boundary**. Every block's first DocID is absolute,
so a block can be decoded without its predecessors — which is the entire point of
the metadata below, since a skipper that had to decode every preceding block to
learn where this one starts would be skipping nothing. It costs at most three
bytes per block, and retrofitting it would cost a format migration.

Term strings are **not** in this file — they live in the terms index, and entries
are located by the offsets recorded there.

`maxDocID`, `maxTF` and `minDocLen` are [D-001](DECISIONS.md): everything
block-max WAND needs, written from the start because retrofitting them is a
migration while writing them costs three varints per block. **No query reads them
before milestone 5.** They are re-derived from each block's contents and compared
on every `Open`, which is what stops an unread field from rotting.

Block size 128 is convention from the block-max WAND literature, not a
measurement — marked `ponytail:` in the source.

### terms

```
term count      uvarint
per term:
  term          string       strictly ascending
  offset        uvarint      absolute file offset of the term's postings entry
```

This is milestone 3's seek structure and no query uses it yet. It is nevertheless
**load-bearing today**: the postings file holds no term strings, so a segment
cannot be read without it. That is deliberate — an index nothing reads is an index
that rots, and this one cannot.

## 5. What a reader rejects

Bytes from disk are a trust boundary. Every failure is an error wrapping
`ErrCorrupt` or `ErrBadVersion`; none is a panic, and none is an index that looks
plausible but breaks an invariant a scorer relies on.

| Rejected | Why it matters |
|---|---|
| Checksum mismatch, truncation, trailing bytes | Damage, or a file this encoder did not write |
| Wrong magic, wrong kind, unknown version | Foreign file, misplaced file, future format |
| Version 1 written in more than one byte | `binary.Uvarint` decodes `0x81 0x00` as 1 too. The header would be seven bytes while `segHeaderLen`, and every offset the terms index records against it, still says six |
| A section file the manifest names but that is not on disk | Damage, reported as `ErrCorrupt` and never as `fs.ErrNotExist`, so a caller's "nothing committed yet" branch cannot overwrite a damaged index |
| Empty or duplicate key | `Add` refuses both; a restored index must not hold what a live one cannot |
| NaN or infinite vector component | `scorer/vector` skips re-checking documents because `ErrNonFiniteVector` promised this |
| Mixed vector widths | Mixed embedding models |
| meta disagreeing with docs | The BM25 snapshot must describe its own corpus |
| Postings not strictly ascending, or naming a document past the corpus | Block skipping and the `TopK` tiebreak both assume dense ascending DocIDs |
| Frequency of 0, or above what the document has left to account for | `Add` writes neither. The bound is the document's remaining token budget rather than `docLen` itself: bounding each posting on its own would let two terms each claim one occurrence in a one-token document, and BM25 would divide real frequencies by a length that never held them. Checking the budget before adding is also what stops the running sum from wrapping — four frequencies near `MaxInt` otherwise land back on a total that agrees with `docLen` |
| A document's frequencies summed across all terms falling short of its stored length | Every token `Add` saw became exactly one posting increment, so the two must agree exactly |
| A DocID delta whose sum overflows uint64 | The wrapped id lands back inside the corpus and passes every later check while breaking ascending order |
| Block metadata contradicting block contents | The D-001 rot check |
| Terms unsorted, or an offset not landing on its entry | Milestone 3 would inherit a broken seek structure |
| A non-final block that is not full | Block and posting counts would stop agreeing |
| A block whose first DocID does not exceed the previous block's last | The block continued a delta chain instead of starting absolute |
| A segment directory or section file that resolves outside the index directory | Both readers work through an `os.Root`, so a symlink planted where a segment belongs is refused by the OS, not by a check that a rename could race |
| A plain file standing where the manifest names a segment directory | A manifest naming a segment has already said this directory is an index, so the entry not being a directory is a foreign or damaged layout. Reported as `ErrCorrupt` rather than the raw `ENOTDIR`, alongside the missing-directory case. A directory that is genuinely there and still will not open is the filesystem refusing us, not corruption, and reports as itself |
| A manifest whose generation already names the segment the next commit would write | `Commit` clears a half-written segment directory before writing it; aimed at the live segment that clears the published commit |
| A generation of `MaxUint64` | The counter advances one commit at a time, so the largest value it can hold is one no writer reached. It is also what `Commit`'s `generation + 1` would wrap to zero on, aiming the pre-write `RemoveAll` at `seg-000000` and then publishing a generation below the one it replaced |

**What a reader deliberately does not check** is that a term matches the text of
the documents it names: nothing re-tokenizes `Text` to compare per-document term
frequencies. A doctored file can therefore file a document's postings under a
token its text does not contain, and text search will return it for that token.
That is accepted, for the same reason `docLen` is stored rather than recomputed —
a segment records what *was* indexed, not what this build's tokenizer would index
today, and a reader demanding the two agree would refuse every segment written
before a tokenizer change. The invariants above are the ones the scorers actually
rest on; term-to-text correspondence is not one of them, and buying it would cost
a full re-tokenization of the corpus on every `Open`.

## 6. Atomicity and durability

`Commit` writes the segment's four files, fsyncs each, fsyncs the segment
directory, then renames a temporary manifest over `MANIFEST`.

**Guaranteed against process death, on POSIX.** The rename is atomic there, so a
crash at any point leaves either the previous generation or the new one — never a
mix, and never a partly visible segment. A segment written but not yet named by a
manifest is indistinguishable from one that was never written.

The guarantee is the platform's, not weft's. Go's `os.Rename` contract does not
promise atomic replacement everywhere it compiles — Windows in particular — and
weft calls no platform-specific replacement primitive. Linux and macOS are what
milestone 2 claims and what its tests run on; anywhere else the commit point is
as atomic as that system's rename and no more.

**Best-effort against power loss.** Contents and directory entries are fsynced,
but weft uses no platform-specific write barrier (no `F_FULLFSYNC` on macOS, no
device cache flush), and directory syncing is unsupported on some systems, where
the error is deliberately ignored. A filesystem that reorders aggressively can
therefore still lose a commit that `Commit` reported as done. Tests pin the
process-crash guarantee; the power-loss case is documented, not tested.

`Commit` refuses to run against a corrupt manifest rather than guessing the next
generation, because writing on top of a directory in an unknown state could orphan
a commit the caller believes exists.

**A first commit has to establish that the directory is weft's.** `Commit` deletes
every `seg-*` entry it finds — its own target before writing it, the rest in the
sweep afterwards — and a manifest is what proves those names are weft's own debris.
With no manifest there is nothing to say that `seg-000001` is debris rather than a
caller's directory sitting under a name weft happens to reserve, so `Commit` aimed
at a home or documents directory would recursively delete data it never wrote. A
commit that crashed before its rename must still be recoverable, so the test is
what such a commit leaves behind rather than mere absence: a `seg-*` entry may
exist, but only as a real directory holding nothing but section files. Anything
else — a stray file inside it, or a symlink standing at the name — and the commit
refuses before it mutates anything at all.

One writer at a time. `Commit` is safe alongside `Add` and queries — it takes the
same read lock a query does — but not alongside another `Commit` on the same
directory. `Open` performs no deletions and no writes, so opening a directory
while it is being committed to cannot damage it; a reader that loses the race
against `Commit`'s sweep of the previous generation sees an error and can retry.

**Nothing weft opens leaves the index directory.** `Commit` and `Open` both work
through an `os.Root` on that directory, so every path below it — segment
directories and section files alike — is resolved by the OS with the guarantee
that it stays beneath the root. A symlink standing where `seg-000007` belongs is
refused rather than followed, and the refusal is not a check of weft's that a
rename could race.

Two things that follows from, and one it does not:

- The manifest's name check and the root do different jobs. `seg-000007` is a
  syntactically perfect name whether the entry standing there is a directory or a
  link into somebody else's index; the first question is about the manifest's
  bytes, the second about the filesystem.
- The temp manifest is additionally created with `O_EXCL`, because it sits at a
  predictable path: without it, a symlink planted at that name would be written
  through, which is the difference between "can write in the index directory" and
  "can overwrite any file this process can reach".
- It is **not** a claim that the directory's contents are trustworthy. Somebody
  who can write there can plant a whole valid segment, and weft would read it —
  which is why every value in it is validated anyway.

## 7. Changing this format

1. **Unknown versions are refused, never guessed.** "Probably compatible" is how a
   wrong index gets loaded silently.
2. **Any change to what the bytes mean bumps `formatVersion`** — a new field, a
   retyped field, a reordered field, a relaxed invariant.
3. **The frame does not change.** Magic, version and kind must stay where they
   are, or a future reader cannot even reach the version in order to reject it.
4. **Milestone 3 does not need a bump for multiple segments.** The manifest
   already carries a list, and the segment format is per-segment.
5. **Migration, when it comes, reads the old version and writes the new one.**
   There is no in-place upgrade path, and version 1 has no ancestor to migrate
   from.

## 8. Known limits

| Limit | Value | Where it goes |
|---|---|---|
| Documents per index | 2³²−1 | `DocID` is uint32; `Add` refuses past it |
| Segments per generation | 1 | Milestone 3 |
| Commit cost | O(corpus) — the whole corpus is rewritten | Milestone 3 |
| Load cost | O(corpus) — `Open` reads everything into memory | Milestone 3's lazy loading, which the terms index exists for |
| Deletion | Not supported | Tombstones and generations ([FINDINGS §4.3](FINDINGS.md)) |
| DocID namespacing | None — a DocID means nothing outside its index | Milestone 3, when multiple segments force it ([FINDINGS §3.4](FINDINGS.md)) |
