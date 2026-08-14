# Changelog

What a caller has to change. Not what changed — `git log -p` already says that, and each tag's release notes are generated from the pull requests that went into it.

Three things can force work on your side, and each is decided by a file rather than by this document:

| What | Where it is decided | How you find out |
|---|---|---|
| `pkg/engine`'s exported API | `pkg/engine/testdata/engine_api.txt` | your build stops compiling; that file's diff between the two tags says what moved |
| The on-disk format | `formatVersion` in `pkg/engine/segment.go` | `Open` returns `ErrBadVersion` |
| The minimum Go version | the `go` line in `go.mod` | `GOTOOLCHAIN=auto`, the default, downloads that version and builds; `GOTOOLCHAIN=local` refuses |

A release that moved none of the three gets no entry. Absence is the claim.

**The module version and the format version are independent.** `v0.2.0` does not imply format 2, and a format bump does not force a major version, because weft is v0.x and its API can break inside a minor either way. They count two different things: one an API you compile against, one bytes already on your disk.

---

## v0.1.0 — unreleased

First tag. Nothing to migrate from.

| | |
|---|---|
| Exported API | baseline: `pkg/engine/testdata/engine_api.txt` at the tagged commit |
| On-disk format | 1 — [FORMAT.md](docs/FORMAT.md) |
| Minimum Go | 1.26 |

A tag is a commit you can name, not a support promise. [README](README.md#status) says weft is not usable in production, and a version number does not change what the code does.
