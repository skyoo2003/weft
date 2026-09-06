# Security Policy

## Supported versions

`main` is the only thing supported. Fixes land there and reach you in the next release; nothing is backported to an earlier tag, because one maintainer cannot promise to maintain two lines. If a fix matters to you before the release carrying it is cut, depend on the commit.

## Reporting a vulnerability

Use the **Report a vulnerability** button on the [Security tab](https://github.com/skyoo2003/weft/security/advisories/new). Please do not open a public issue for a vulnerability.

If you would rather not use GitHub, or have no account, email <skyoo2003@gmail.com> instead. Both routes reach the same person. The button is preferred only because it keeps the report, the fix and the advisory in one thread instead of in a mailbox, and because a mailbox is the one of the two that can silently drop a message.

Include enough to reproduce it: the input, the calling sequence, and what you expected instead.

## What to expect

One maintainer, no service-level agreement. Reports are read and answered on a best-effort basis. Promising a response time here would be a promise this project cannot keep, so it is not made.

If you get no reply, assume the report was missed rather than dismissed, and send it again.

## What runs without being asked

Two gates look for this class of bug on their own, and neither is a substitute for a report:

- [`make fuzz`](CONTRIBUTING.md#the-gate) — 30 seconds each against the two segment-decoder fuzz targets, on every CI run. It finds crashes, on inputs it happened to generate in half a minute.
- [CodeQL](.github/workflows/codeql.yml) — on every Go change and once a week, asking where a value the file chose reaches an allocation or an index. It finds reachability, on the queries GitHub has written.

Both are shallow in the direction the other is deep, and both stop at the first row of the table below: neither has anything to say about what `Commit` deletes.

## Where the risk actually is

weft is a library. It opens no sockets and authenticates nobody. It does write and delete files in a directory you hand it. Three places are worth your attention:

| Surface | Why |
| --- | --- |
| The persisted segment format | `Open` parses a block-structured file with varint headers ([docs/FORMAT.md](docs/FORMAT.md)). A malformed or hostile file reaching the decoder is the most likely source of a panic, an out-of-range read, or an allocation driven by a length the file chose. |
| What `Commit` deletes | `Commit` creates the directory you name and recursively removes every `seg-*` entry and `MANIFEST.tmp` it finds there. `refuseForeignEntries` is what stops a first commit into, say, a home directory from deleting data weft never wrote. A path that gets past that guard, or a link that escapes the `os.Root` both readers and the writer work through, is in scope. |
| Caller-supplied vectors and documents | The library does validate: `Index.Add` rejects non-finite components (`ErrNonFiniteVector`) and vectors whose width disagrees with the corpus (`ErrDimMismatch`), the vector scorer rejects a non-finite query norm, and the segment decoder checks again on the way back in. Anything that gets past one of those checks is in scope. Size is the part nobody checks on this path: a corpus you built yourself that is too large for RAM is a [documented limitation](docs/LIMITATIONS.md), not a vulnerability. That carve-out is about your own corpus and does not extend to a file you did not write — `Open` reads each section fully into memory before it can bound anything, so allocation driven by a hostile segment belongs to the first row and is in scope. |
