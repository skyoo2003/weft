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
