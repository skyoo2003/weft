# Security Policy

## Supported versions

| Version | Supported |
|---|---|
| `main` | Yes |
| Anything else | No |

There are no releases yet, so there is nothing to backport to. Fixes land on `main`.

## Reporting a vulnerability

Email **skyoo2003@gmail.com**. Please do not open a public issue for a vulnerability.

Include enough to reproduce it: the input, the calling sequence, and what you expected instead.

## What to expect

One maintainer, no service-level agreement. Reports are read and answered on a best-effort basis. Promising a response time here would be a promise this project cannot keep, so it is not made.

If you get no reply, assume the report was missed rather than dismissed, and send it again.

## Where the risk actually is

weft is a library. It opens no sockets, authenticates nobody, and performs no privileged operation. Two places are still worth your attention:

| Surface | Why |
|---|---|
| The persisted segment format | `Open` parses a block-structured file with varint headers ([docs/FORMAT.md](docs/FORMAT.md)). A malformed or hostile file reaching the decoder is the most likely source of a panic or an out-of-range read. |
| Caller-supplied vectors and documents | The library does validate: `Index.Add` rejects non-finite components (`ErrNonFiniteVector`) and vectors whose width disagrees with the corpus (`ErrDimMismatch`), the vector scorer rejects a non-finite query norm, and the segment decoder checks again on the way back in. Anything that gets past one of those checks is in scope. Size is the part nobody checks — a document or vector large enough to exhaust memory is a [documented limitation](README.md#limitations), not a vulnerability. |

Reports that the corpus must fit in memory are not vulnerabilities — that is a documented limitation ([README — Limitations](README.md#limitations)).
