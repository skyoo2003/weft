# Security Policy

## Supported versions

There are no releases yet, so there is nothing to backport to. Fixes land on `main`, and `main` is the only thing supported.

## Reporting a vulnerability

Email **skyoo2003@gmail.com**. Please do not open a public issue for a vulnerability.

GitHub's private vulnerability reporting is not enabled on this repository, so the Security tab has no **Report a vulnerability** button. Email is the channel that works.

Include enough to reproduce it: the input, the calling sequence, and what you expected instead.

## What to expect

One maintainer, no service-level agreement. Reports are read and answered on a best-effort basis. Promising a response time here would be a promise this project cannot keep, so it is not made.

If you get no reply, assume the report was missed rather than dismissed, and send it again.

## Where the risk actually is

weft is a library. It opens no sockets and authenticates nobody. It does write and delete files in a directory you hand it. Three places are worth your attention:

| Surface | Why |
|---|---|
| The persisted segment format | `Open` parses a block-structured file with varint headers ([docs/FORMAT.md](docs/FORMAT.md)). A malformed or hostile file reaching the decoder is the most likely source of a panic, an out-of-range read, or an allocation driven by a length the file chose. |
| What `Commit` deletes | `Commit` creates the directory you name and recursively removes every `seg-*` entry and `MANIFEST.tmp` it finds there. `refuseForeignEntries` is what stops a first commit into, say, a home directory from deleting data weft never wrote. A path that gets past that guard, or a link that escapes the `os.Root` both readers and the writer work through, is in scope. |
| Caller-supplied vectors and documents | The library does validate: `Index.Add` rejects non-finite components (`ErrNonFiniteVector`) and vectors whose width disagrees with the corpus (`ErrDimMismatch`), the vector scorer rejects a non-finite query norm, and the segment decoder checks again on the way back in. Anything that gets past one of those checks is in scope. Size is the part nobody checks on this path: a corpus you built yourself that is too large for RAM is a [documented limitation](README.md#limitations), not a vulnerability. That carve-out is about your own corpus and does not extend to a file you did not write — `Open` reads each section fully into memory before it can bound anything, so allocation driven by a hostile segment belongs to the first row and is in scope. |
