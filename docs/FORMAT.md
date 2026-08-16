# On-disk format, version 3

`pkg/engine/segment.go`, `pkg/engine/seek.go`, `pkg/engine/ivf.go` and
`pkg/engine/persist.go` are what the bytes actually obey. Where this document and
those files disagree, the document is wrong.

Milestone 2 wrote version 1; milestone 3a replaced it with version 2; milestone 3b
appended one section and made it version 3. It is written down because a format is
the one thing in weft that cannot be quietly rewritten — code is replaceable, a
file on a user's disk demands a migration.

**Versions 2 and 3 are both read. Version 1 is refused, not migrated.** `Open`
reports `ErrBadVersion` for v1. That reason is not technical: weft has no users,
and the only v1 directory that existed was rebuildable from its sources. That
argument rests on a user count and is therefore available exactly once —
[D-007](DECISIONS.md) records it, and §7 says what it obliged version 3 to bring
instead.

**What version 3 brought is the reader, not a converter.** v3 is v2 plus the `ivf`
section and nothing else, and the fallback a segment without a partition needs —
*every id is a candidate* — is the same one a pending segment and a segment below
4,096 documents already need. So reading v2 costs a branch that had to exist
anyway. `Index.Merge` is the converter in practice: it rewrites the run it
collapses with the current writer, so a v2 generation is upgraded by the
maintenance an index already performs, with no migration command and no directory
that has to be rebuilt.

What v1 could not express, and why the change was bytes rather than code: its
`docs` section was a bare run of variable-length records and its key map was
derived by reading all of them, so neither a `DocID` nor a `Key` could reach its
document without decoding every document in front of it. No arrangement of a lazy
reader fixes that.

---

## 1. Directory layout

```text
dir/
  MANIFEST              the only entry point; one atomic rename publishes a commit
  seg-000007/           one generation, immutable once the manifest names it
    meta                collection statistics — the BM25 snapshot
    docs                every document, in DocID order
    postings            the term dictionary, block-structured
    terms               term → absolute offset into postings
    docoff              DocID → (offset into docs, token count)   ← v2
    keys                sorted Key → DocID                        ← v2
    ivf                 centroids + inverted lists                ← v3
```

**The version decides the section list.** A v2 segment has six files, a v3 segment
has seven, and every frame inside one segment must declare the same version. A v3
segment missing `ivf` is damage, not an older segment; a v2 segment with an `ivf`
file beside it has a file nothing names. `meta` is opened first for exactly this
reason — its frame is what says which list applies.

A commit writes **one segment holding what was added since the last commit**, and
leaves the previous generations' files untouched. A segment stores ids local to
itself, counting from zero, and the manifest says where it sits in the index — so
a segment's bytes do not depend on what was committed before it, which is what
lets an old generation survive a new one byte for byte.

`Index.Merge` collapses the oldest run of segments into one when the count passes
eight. Merging *adjacent* segments is concatenation: every document keeps the id it
had, so nothing is renumbered. That is not a convenience — `engine.TopK` breaks
ties on `DocID`, so a merge that moved ids would move rankings.

The manifest storing a *list* is version 1's doing, from [D-003](DECISIONS.md).
Incremental commit extends its contents and not its shape, which is the one thing
about v1 that survived contact with this milestone.

A segment directory the manifest does not name does not exist. Nothing reads it,
and `Commit` deletes it on its way past. `Open` leaves it alone: "unnamed" is also
true of the segment a commit still in flight has written but not yet published, so
a reader that swept would delete a live writer's work.

## 2. File frame

Every file, the manifest included, wears the same frame:

| Field | Bytes | Notes |
| --- | --- | --- |
| magic | 4 | `weft` |
| format version | uvarint | `3` when written, `2` or `3` when read; one byte until version 128 |
| kind | 1 | see below |
| payload | — | section-specific, below |
| checksum | 4 | CRC-32 Castagnoli, little-endian, over everything above |

`kind` is `1` meta, `2` docs, `3` postings, `4` terms, `5` manifest, `6` docoff,
`7` keys, `8` ivf. It exists
because a checksum tells intact bytes from damaged ones and nothing more: without
a kind byte, a healthy `docs` file copied over `meta` would be parsed rather than
refused.

Readers verify in this order, and the order is deliberate:

1. **magic** — a file that was never weft's reads as corrupt, not as a version problem.
2. **checksum** — so a flipped bit in the version byte cannot masquerade as a version mismatch.
3. **version** — checked only once the bytes are known intact.
4. **kind** — catches an intact file standing at the wrong path.

**`Open` skips step 2, and `Scrub` is where it went.** The frame checksum covers
every byte of a file, so computing it costs a full read — the cost lazy loading
exists to remove. One consequence follows and is not hidden: on the `Open` path a
damaged version byte reports `ErrBadVersion` rather than `ErrCorrupt`, because
nothing has told the reader those bytes are damaged. `Scrub` runs all four steps
in order and calls it corruption.

### Unit checksums

Each `docs` record, each `postings` block and each `terms` entry carries its own
CRC-32C, four bytes, immediately after it. A reader that touches one unit verifies
one unit, which is what makes lazy reading and integrity compatible.

Every unit checksum is **seeded** with what names the unit — a `DocID` for a
record, its own absolute file offset for a block, its index for a terms entry —
folded in as eight little-endian bytes before the unit's own content. The seed is
the part that matters. A document record does not carry its `DocID`; its position
did, and `docoff` is what turns an id into that position. Trust the table and one
damaged entry yields a healthy record decoded under someone else's id — a
plausible wrong answer rather than an error. Binding the id in makes the record
prove which document it is.

The frame checksum still covers the unit checksums, so `Scrub` catches damage
anywhere including in the checksums themselves.

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

```text
generation      uvarint     strictly increasing; a write names its directory from it
segment count   uvarint
  segment name  string      e.g. "seg-000007"
  base          uvarint     the first DocID this segment owns
  count         uvarint     how many documents it holds
```

The bases must tile `[0, total)` contiguously and in ascending order. A list that
did not would give two segments overlapping ids, and a reader would answer with
whichever it walked into first — a wrong document, not an error — so this is
checked while the list is read rather than afterwards.

Some published segment must be named for the current generation, and none may be
named for the next one: each write clears `seg-<gen+1>` before using it, so a
manifest already naming it would aim that removal at live data. Version 1 could say
something stronger — the *last* segment is the generation's — because a commit
published exactly one; a merge publishes its result at the front, since it replaces
the oldest run.

Segment names come off disk and are therefore validated, not trusted: a name must
start with `seg-`, contain no path separator, and equal its own basename. A
manifest naming `seg-../../etc` is refused rather than followed.

### meta

```text
document count  uvarint     ≤ 2³²−1, the DocID ceiling
total length    uvarint     Σ document token counts
vector width    uvarint     0 when no document carries a vector
```

These three are the collection statistics BM25 reads, written under the same read
lock as the documents, so a commit is a point-in-time snapshot with its statistics
intact ([FINDINGS §4.4](FINDINGS.md)). `Open` cross-checks all three against the
`docs` file and refuses a segment whose meta disagrees with its own documents.

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

```text
term count      uvarint
per term:
  term          string       strictly ascending
  offset        uvarint      absolute file offset of the term's postings entry
```

This is milestone 3's seek structure and no query uses it yet. It is nevertheless
**load-bearing today**: the postings file holds no term strings, so a segment
cannot be read without it. That is deliberate — an index nothing reads is an index
that rots, and this one cannot.

### docoff (v2)

```text
document count  uvarint
  offset        uint64 LE   absolute file offset of the record in docs
  token count   uint64 LE   the same number the record itself carries
```

Fixed width, sixteen bytes an entry, and that is the whole point: entry *i* is at a
computable position, so `Doc(id)` is arithmetic plus one record decode. A uvarint
table would have to be walked from the front, which is the cost being removed.

Eight bytes for the offset rather than four, because a 656 MB `docs` file already
sits one order of magnitude from a `uint32` ceiling and raising it later is a
migration. The token count rides along rather than being read out of the record,
because BM25 asks for a length once per posting and reaching it through the record
would make every posting cost a key, a text and a vector.

The record carries the length too, and the two are compared — a copy nobody checks
is what [D-001](DECISIONS.md) is about.

### keys (v2)

```text
key count       uvarint
  offset        uint64 LE      absolute file offset of the entry, in key order
  (reserved)    8 bytes        written zero; the table shares docoff's 16-byte width
  key           string         the entries themselves, ascending
  document id   uvarint        segment-local
```

The reserved half is padding, not a field: the table reuses `docoff`'s entry width
and has no token count to put beside the offset. Eight bytes a document, kept
because the width is on disk — narrowing it belongs to a version bump, not to a
reader that would then disagree with every v2 index already written.

The offset table makes `Resolve` a binary search rather than a map rebuilt by
reading every document. Ascending order is what the search rests on, so an unsorted
table does not fail — it answers wrongly — and the decoder checks it.

Offsets are computed forward rather than patched back in: an entry's encoded length
is known from the key and the id, and the table's own size from the count. A writer
that had to seek backwards could not stream, and merge cannot buffer a corpus.

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

An IVF-flat partition of the segment's vectors: spherical k-means centroids, and
the segment-local DocIDs assigned to each. `Index.Nearest` ranks the centroids by
inner product with a query, reads the best `nprobe` lists, and returns their
members as the documents worth scoring exactly. **No score is stored and none is
computed here** — the metric belongs to the caller, which is [D-008](DECISIONS.md).

Four decisions here are load-bearing:

- **The section is always written**, `nlist = 0` when the segment holds fewer than
  4,096 documents or no vectors. A section list that varied with the corpus would
  make the rejection table above say "may or may not be here", and every reader
  and the first-commit ownership check would have to know which.
- **Spherical, not plain L2.** Vectors are normalized before training and the
  assignment maximizes an inner product. Ranking is by cosine, and partitioning by
  raw L2 would gather the documents with the largest norms into one list
  regardless of direction — losing exactly the neighbours a cosine query wants.
- **The offset table is fixed width**, for the reason `docoff`'s is: a query
  probes a few lists out of up to a thousand, and reaching list *j* through a
  uvarint table would mean decoding the *j−1* lists in front of it, which is the
  corpus. At eight bytes an entry it is at most eight kilobytes. Offsets are
  computed forward, never patched back in, so the writer still streams.
- **Each list's checksum is seeded with its own number.** A list carries no copy
  of which centroid it belongs to, so a reader following a damaged offset would
  decode a *healthy* list under the wrong centroid. That is not an error, it is a
  plausible wrong candidate set — and a wrong candidate set is indistinguishable
  from ordinary recall loss, which is the one failure an approximate index must
  not be allowed to hide.

`nlist` is `min(⌈√count⌉, 1024)`, from the document count rather than the vector
count. `nprobe` is a constant 64 and is not configurable; `Nearest` raises it on
its own when a query would otherwise return fewer than *k* candidates, so "at
least k" is a contract rather than a tuning exercise. Both numbers are the
reader's policy and neither is stored, so changing `nprobe` needs no rebuild.

**Damage in a list costs speed, not availability.** A list that fails its own
checksum makes the whole segment behave as though it had no partition: every id
becomes a candidate, the answer becomes exact, and the query becomes slow. That is
[D-006](DECISIONS.md)'s rule where absence is the safest answer, and `Scrub` is
what names the damage.

## 5. What a reader rejects

Bytes from disk are a trust boundary. Every failure is an error wrapping
`ErrCorrupt` or `ErrBadVersion`; none is a panic, and none is an index that looks
plausible but breaks an invariant a scorer relies on.

| Rejected | Why it matters |
| --- | --- |
| Checksum mismatch, truncation, trailing bytes | Damage, or a file this encoder did not write |
| Wrong magic, wrong kind, unknown version | Foreign file, misplaced file, future format |
| The version written in more than one byte | `binary.Uvarint` decodes `0x82 0x00` as 2 too. The header would be seven bytes while `segHeaderLen`, and every offset the terms index records against it, still says six |
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
| An entry of the wrong kind at any path in the layout: a plain file or a symlink where the manifest names a segment directory, a directory or a symlink where one of its section files belongs, a directory or a symlink at `MANIFEST` itself | An entry of the wrong kind is a foreign or damaged layout, and every path gets the same judgement — including the entry point, whose raw `EISDIR` would otherwise be neither `ErrCorrupt` nor `fs.ErrNotExist`, leaving a caller with no branch to take. Reported as `ErrCorrupt` rather than the raw `ENOTDIR`, `EISDIR` or path-escape error, alongside the missing-directory and missing-file cases. The kind is asked with `Lstat`, not `Stat`, because `Stat` follows the very link in question. An entry of the right kind that is genuinely there and still will not open is the filesystem refusing us, not corruption, and reports as itself |
| A manifest whose generation already names the segment the next write would produce | `Commit` and `Merge` both clear a half-written segment directory before writing it; aimed at a live segment that clears published data |
| A manifest naming no segment for its own generation | The generation counter and the directory would have stopped describing each other |
| Segment bases that do not tile `[0, total)` contiguously and in order | Two segments would own the same ids, and a reader would answer with whichever it walked into first: a wrong document, not an error |
| A `docoff` or `keys` count disagreeing with `meta` | The seek tables and the statistics BM25 trusts would be describing different corpora |
| A unit whose checksum does not match, seeded with what names it | Covers damage no semantic rule can see — a flipped byte of document text is invisible to every other check here — and covers a record reached through a damaged offset, which would otherwise decode healthily under someone else's id |
| Two sections of one segment declaring different format versions | Two versions are accepted, so "every frame says the same number" stopped being enforced by the version check itself. A segment whose `meta` says 3 and whose `docs` says 2 passes every frame check individually and describes nothing a writer produced — and the section list was chosen from one of those two numbers |
| A v3 segment with no `ivf` file, or a `nlist` above 1024, or centroids not as wide as `meta`'s vectors, or a member count above the segment's document count, or the counts summing past it | The version fixes the section list, and the rest are states the writer cannot produce: one document belongs to at most one list, so the counts partition the segment |
| A non-finite centroid component | It would poison every comparison it takes part in, and the ranking sorts NaN last rather than refusing it — so the query would silently never probe that list |
| An `ivf` list not strictly ascending, naming a document past the segment, or failing its seeded checksum | Refused **by `Scrub`**. On the read path the same conditions make the segment answer as though it had no partition, because a slow exact answer beats an error — see D-006 |
| A generation of `0` or `MaxUint64` | The counter advances one commit at a time from a first commit that publishes `1`, so neither end is a state a writer produced. `0` describes a commit that never happened: accepting it would load a foreign layout, and let the next `Commit` publish over it and then sweep away a `seg-000000` weft never wrote. `MaxUint64` is what `Commit`'s `generation + 1` would wrap to zero on, aiming the pre-write `RemoveAll` at `seg-000000` and then publishing a generation below the one it replaced |

**What only `Scrub` checks.** `Open` verifies the frame header, the manifest and
`meta`, and each unit as it is read. Everything else in the table above — the
whole-file checksums, every document, every posting list, both seek tables in full
— is `Scrub`'s. The gap that leaves is real and worth stating: a unit nothing ever
reads is never verified, so rot in a document no query reaches sits there until
somebody runs `Scrub`. Milestone 2 got this free because `Open` read every byte;
milestone 3 buys it explicitly.

**One thing nothing checks, stated plainly.** No reader verifies that every
vector-bearing document appears in some `ivf` list. A document missing from the
partition is invisible to a vector query, and the cost of that is recall — the
same currency an approximate index spends by design, and not separable from it by
any rule available on either path. Buying the check would mean carrying one bit
per document out of the `docs` walk, and the answer it would give is "this index
recalls slightly less than it should", which `weft-eval recall` measures directly
against a brute-force scan.

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

`Commit` writes the segment's six files, fsyncs each, fsyncs the segment
directory, fsyncs the index directory so the segment directory's own entry is
durable before anything names it, then renames a temporary manifest over
`MANIFEST` and fsyncs the index directory again. Syncing only the inside of the
segment would leave the rename free to land first, and a manifest naming a
directory whose entry never made it is the mixed state the rename exists to rule
out.

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
at a home or documents directory would recursively delete data it never wrote. The
same goes for `MANIFEST.tmp`, the other name `Commit` deletes on sight. A commit
that crashed before its rename must still be recoverable, so the test is what such
a commit leaves behind rather than mere absence: a `seg-*` entry may exist, but
only as a real directory holding nothing but regular section files, and
`MANIFEST.tmp` may exist too — and every one of those files has to carry weft's
magic, because a reserved name and a file type are things a caller's own data can
have by coincidence and four bytes of magic are not. A file the writer created but
never filled satisfies that as well: it buffers until close, so its debris is
empty or a prefix of the frame. Anything else — a stray entry inside the segment,
a *directory* named `meta`, which the recursive clearing would take everything
beneath, a symlink standing at any of those names, or a file under one of them
that weft did not write — and the commit refuses before it mutates anything at
all. The magic is the ownership signal and the only one available here: the kind
byte says which section a file is rather than whose it is, and a torn write may
stop before reaching it. A frame intact enough to carry the magic but broken past
it is weft's own debris, and the reader's checksum is what refuses it as an index.

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
4. **Multiple segments needed no bump, and lazy reading did.** The manifest
   already carried a list and the segment format is per-segment, so incremental
   commit and merge changed contents rather than shape — [D-003](DECISIONS.md)
   designed for that and it held. What forced version 2 was smaller and inside a
   single segment: `docs` was positional and the key map was derived, so neither a
   `DocID` nor a `Key` could reach its document without decoding the corpus.
5. **Migration, when it comes, reads the old version and writes the new one.**
   There is no in-place upgrade path.
6. **Version 2 skipped that, and no later version may.** v1 is refused rather than
   converted, on the single ground that weft had no users and the one v1 directory
   in existence was rebuildable. That is an argument about a user count, so it
   expires the moment somebody has an index they cannot rebuild —
   [D-007](DECISIONS.md). A version 3 owes either a converter or a reader that
   understands both.
7. **Version 3 paid that debt with the reader, and it was nearly free.** v3
   appends `ivf` and changes nothing else, and a segment without a partition has
   to be readable regardless — a pending segment has none, a segment below 4,096
   documents has none, and a damaged partition is treated as none. So "v2 is a
   segment with no partition" reuses a branch that already existed, where a
   converter would have been a command, a rebuild, and a directory in two states
   while it ran. `Merge` then upgrades in the background, because it rewrites the
   run it collapses with the current writer. The lesson for version 4 is the one
   that made this cheap: **append a section rather than changing one**, and the
   old version stays readable by construction. A version that retypes or reorders
   an existing field gets no such discount and owes the converter.

## 8. Known limits

| Limit | Value | Where it goes |
| --- | --- | --- |
| Documents per index | 2³²−1 | `DocID` is uint32; `Add` refuses past it |
| Deletion | Not supported | Tombstones and generations ([FINDINGS §4.3](FINDINGS.md)) |
| DocID namespacing | None — a DocID means nothing outside its index | Unresolved; a DocID is meaningful only inside one index directory |
| Vector search is approximate | recall@10 = 0.992 against a brute-force scan on the evaluation corpus | The screw is `nprobe`, and it is not exposed. [EVAL §5](EVAL.md) carries the curve |
| A vector query's working set | 210 MiB per query of a 626 MiB `docs` section | The partition cut the arithmetic 5.6× and the bytes 3.0×. Why the second number is so much worse than the first, and what would actually fix it, is [FINDINGS milestone 3b](FINDINGS.md) |
| Build cost | +68 s on the 171,332-document evaluation corpus, for the partition | Constant per commit and per merge, not per query. Below 4,096 documents it is not paid at all |
| `nlist` ceiling | 1024 | The assignment pass is linear in it |
