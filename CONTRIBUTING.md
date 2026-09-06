# Contributing

One maintainer. Replies are slow but real.

Behavior in this repository is covered by the [Code of Conduct](CODE_OF_CONDUCT.md). Vulnerabilities go to [SECURITY.md](SECURITY.md), not to the issue tracker.

## The gate

```bash
make all      # fmt + build + vet + test -race, plus golangci-lint and markdownlint when you have them
```

That needs nothing installed but the Go toolchain, which is deliberate: you can run the whole of it before you have read anything or installed anything. The two linters are the conditional part — with `golangci-lint` on your `PATH`, `make all` runs it; with `markdownlint-cli2` or just `npx`, it runs the docs lint too; without either, each prints a SKIP line naming the install command and carries on. [CI](.github/workflows/ci.yml) calls this same target rather than copying its commands, so neither can drift from the other. The target is shared; the environment is not. CI runs one platform, `ubuntu-latest`, at go.mod's Go version, and `test -race` needs a C toolchain, so a local failure CI would never have seen is possible in the other direction.

Both linters are in `all` because of what happened without them: for five commits `make all` passed a tree CI rejected, because the only gates reading [.golangci.yaml](.golangci.yaml) and [.markdownlint.yaml](.markdownlint.yaml) lived in CI — and the Go one failing first meant the docs one had never run at all, so five documents reached 76 findings unread. A local run still cannot promise a CI pass — CI installs pinned versions and yours may be newer, which `make lint` warns about — but it can stop CI from being where a finding is first seen.

Four more checks run in CI and are Makefile targets too, kept out of `make all` because each costs a tool to install or a minute of wall clock:

```bash
make spdx         # every .go file carries its licence line; make spdx-fix adds them
make bench-build  # vet + test the bleve comparison, which is its own module
make grpc-build   # vet + test the gRPC server, which is the second one
make fuzz         # 30s each against the five fuzz targets
```

`make lint` and `make lint-docs` are those same two runs asked for by name: they fail rather than skip when the tool is missing, and `make lint` covers `bench/` and `grpc/` too.

`bench-build` and `grpc-build` are separate from `make all` for a structural reason rather than a cost one: each is a nested module, so neither `go build ./...` nor `golangci-lint run ./...` at the root descends into it. Without a target naming them, the bleve half of milestone 5's comparison and the whole of the gRPC surface would rot unnoticed between the runs that use them — and for `grpc/` that means a broken server could reach a tag.

`make lint` needs `golangci-lint` and `make lint-docs` needs `markdownlint-cli2` or an `npx` to fetch it with; each target says so and names the install command rather than failing obscurely. `make fuzz` needs nothing but time, and it is the one most likely to find something no test covers — [SECURITY.md](SECURITY.md) names the segment decoder as the first place a hostile file lands, and the three parsers under `internal/opensearch` are where a hostile *request* lands. There is an optional [pre-commit config](.pre-commit-config.yaml) that runs `make all`, `make spdx` and `make lint`; nothing requires it, and CI does not use it.

Two further targets are checks that CI does not run, because each needs a Python client CI does not install: `make compat` drives `weftd` with `opensearch-py`, and `make compat-grpc` drives `weftg` with a stock `grpcio` client using stubs it generates from `weft.proto` itself. Both skip rather than fail when the client is absent, and the Makefile gives the virtualenv recipe above each one. `PYTHON=` overrides the interpreter so neither needs a global install.

One gate has no Makefile target and no local half: [CodeQL](.github/workflows/codeql.yml), which runs on its own workflow against Go changes and once a week. It is absent from `make all` because it cannot be there — it builds a dataflow database before it can ask anything, which is minutes rather than seconds. It asks a different question from `golangci-lint` too: not whether a file is correct Go, but where a value the caller chose ends up. Its findings land on the Security tab rather than in a pull request check, so the way you meet it is by being told, not by a red build.

## Every target

Generated from the `Makefile`, which is the source of truth. `make help` is not a thing here; this table is.

| Target | What it does |
| --- | --- |
| `make`, `make all` | fmt + build + vet + `test -race`, plus both linters when they are installed |
| `make arch` | The milestone 1 pass/fail line, verbosely, plus the two golden-file tests |
| `make deps` | The two architecture properties cheap enough to check by hand: zero dependencies, and fusion sees no scorer |
| `make lint`, `make lint-docs` | The two linters by name — these fail rather than skip when the tool is missing |
| `make spdx`, `make spdx-fix` | Every `.go` file carries its licence line; `-fix` adds the missing ones |
| `make fuzz` | 30 seconds each against the five fuzz targets: the segment decoder and section parser in `pkg/engine`, the DSL, native and bulk parsers in `internal/opensearch` |
| `make run`, `make example` | The interactive demo; the minimal embedding under `examples/` |
| `make serve`, `make serve-grpc` | `weftd` on `127.0.0.1:9200`, `weftg` on `127.0.0.1:9201`. Both default to `.weftd-data`, so two terminals are two surfaces over one index rather than two empty ones |
| `make compat`, `make compat-grpc` | `opensearch-py` drives `weftd` unmodified; a stock `grpcio` client drives `weftg` from stubs it generates from `weft.proto`. Both skip without the Python client; `PYTHON=` picks the interpreter |
| `make grpc-build` | vet + test the gRPC module, which `./...` at the root never reaches |
| `make eval` | Milestone 4's published nDCG table, reprinted from an already-built index |
| `make eval-full` | Adds the degeneracy diagnostic, the frozen arms and the 28-configuration sweep. Slower |
| `make eval-data` | Refuses to call corpus preparation complete until the query vectors exist — without them `text+vector` arms silently measure text only |
| `make recall` | Overlap with a brute-force scan, which is what nDCG cannot see about an approximate vector index |
| `make bench` | Milestone 5's latency ladder. Long: the lowest rung alone sends 10,000 queries |
| `make bench-preflight` | The ladder's top rung alone, about a minute. Run it first and read two lines — three ladders have been spent on machines that could not reproduce their own published figures |
| `make bench-http` | The same ladder through a socket. Starts `weftd`, drives it over HTTP, stops it, so the only difference from `bench` is the surface and the second process |
| `make bench-build`, `make bench-compare`, `make bench-head` | The bleve comparison, which is its own module: vet + test it; run the ladder against bleve; the head-to-head microbenchmark milestone 22 reads |
| `make changelog`, `make changelog-new`, `make changelog-check` | Render `CHANGELOG.md`; start an entry — one sentence, two at most, which `changie` refuses past 500 characters; fail if it was hand-edited |
| `make docs-site`, `make release-check`, `make clean` | Render the docs site from `/docs`; dry-run the release pipeline before the tag is unwithdrawable; remove build output |

`fmt`, `build`, `vet` and `test` are the four `all` is made of and can be run by name; `lint-if-present` and `lint-docs-if-present` are how `all` reaches the two linters without requiring them, and are not meant to be typed.

Everything needing a prepared corpus — `eval`, `eval-full`, `recall`, `bench`, `bench-preflight`, `bench-http`, `bench-compare` — needs data that is not in the repository. [EVAL.md §7](docs/EVAL.md) lists the downloads and the one-time `weft-eval prepare` step.

## What not to break

Two assertions are decided by tests rather than by a reviewer:

- Fusion is invariant to scorer count.
- Neither `engine/` nor `fusion/` may import a scorer package.

The second holds for every scorer automatically, yours included: it reads the import graph, so it needs no baseline and no new test. Note that it covers `engine/` as well as `fusion/`, and that `engine` is asserted to import no weft package at all.

The first does not generalize by itself. `TestAddingAFourthScorerDoesNotChangeTheCallShape` and `TestAnyNumberOfScorersFuses` name the four scorers in the tree, so nothing runs yours until you add it to those two slices in `pkg/engine/architecture_test.go`. Add it there.

They are ordinary tests in `pkg/engine`, so `make all` — and therefore CI — already runs them. `make arch` runs them by name and verbosely, along with two you will meet only by tripping: `TestNoExternalDependencies`, and two golden-file tests. `TestEngineAPISurfaceIsUnchanged` fails when `engine`'s exported surface changes and asks you to record that cost in `docs/FINDINGS.md`; `TestPublicAPISurfaceIsUnchanged` does the same for `fusion` and every scorer package, which a caller builds directly. Both refresh with `WEFT_UPDATE_GOLDEN=1`. The second discovers packages instead of listing them, so adding a scorer trips it — that is not a hurdle, it is the addition becoming visible. Both read the source rather than a build, so they answer for the platforms named in `apiContexts` — `linux/amd64`, `darwin/arm64`, `windows/amd64` — and a declaration some of those cannot see is recorded with the ones that can. A fifth platform is a line in that list. `make arch` drops `-race`, so it is a different question from `make all`, not a louder version of it.

The line-count figure is a measurement, not a budget you have to fit under — with one exception worth knowing before it bites you. `TestFourthScorerIsUnderOneHundredLines` measures `scorer/recency` and nothing else, so a fifth scorer twice that size still passes, and milestone 3's ANN scorer will not fit under 100 lines either. For `scorer/recency` itself it is a hard failure at 100, `recency.go` counts 99, and the count includes comments and blank lines: one added line there turns `make all` red. That is deliberate — the figure is a published claim about this project, so changing the file means changing the claim, and the pull request is where you say so. The one thing the count does *not* include is the SPDX header and the blank line under it, because `make spdx` puts those on every file in the repository and charging a scorer for a repository-wide licensing decision would measure the wrong thing. If your own scorer is much larger, the pull request is also where you say why.

## Adding a scorer

The main extension path is written down once, in [docs/SCORERS.md](docs/SCORERS.md): implement `engine.Scorer` and nothing in `engine/` or `fusion/` should need to change. If your change does need to touch them, that is the interesting part of the pull request — lead with it.

## Pull requests

Fill in [the template](.github/PULL_REQUEST_TEMPLATE.md). Its five sections ask what changed and why, how you verified it, what deserves attention, and what might bite later; answering them is most of the review.

A change a caller can notice also needs a changelog entry, and `make changelog-new` writes one into `changes/unreleased/`. It is one sentence — two when the second says what you have to do about the first — and `changie` refuses a body past 500 characters, which is that rule as something the command enforces rather than something a reviewer has to notice. What belongs in the entry is the claim; the argument for it belongs in this pull request and the mechanism in [docs/](docs/), both of which an entry can link to rather than repeat.

Commit messages: say why. `git log -p` already says what.

No CLA and no DCO sign-off. Contributions are under [Apache-2.0](LICENSE), the license already on this repository.

## Before a large change

Open a [proposal](https://github.com/skyoo2003/weft/issues/new?template=proposal.yml) first. Not for permission — to find out whether the thing you are about to build is already recorded as rejected in [docs/DECISIONS.md](docs/DECISIONS.md) or as unverified in [docs/FINDINGS.md](docs/FINDINGS.md).

Neither document repeats anything readable from the code. If a change contradicts one of them, that document is part of the change.

Bugs have [their own form](https://github.com/skyoo2003/weft/issues/new?template=bug.yml), and it asks for a commit SHA because there is nothing else to name yet. A question needs no form at all — open a blank issue.

A `priority:` label is the maintainer reading a queue, not a commitment about when anything ships. Nothing here carries a response time, and [SECURITY.md](SECURITY.md) says the same about vulnerabilities.

## Stability

The API can break while this is v0.x. Changes that are expensive to reverse get a `DECISIONS.md` entry; everything else can move without ceremony.

Supported Go is the version in `go.mod` and newer. Newer is untested rather than unsupported: [the gate](#the-gate) pins that one line, so there is one number rather than a matrix.

## Cutting a release

[RELEASE.md](RELEASE.md). It is four steps, only one of them irreversible, and it lives in its own file so that there is one copy of it rather than two that drift.

Two things there are worth knowing before you need them: `changie batch` is what turns `changes/unreleased/` into a version, and the tag push is the point of no return — the Go module proxy does not forget a version it has served.
