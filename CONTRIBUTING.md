# Contributing

One maintainer. Replies are slow but real.

Behavior in this repository is covered by the [Code of Conduct](CODE_OF_CONDUCT.md). Vulnerabilities go to [SECURITY.md](SECURITY.md), not to the issue tracker.

## The gate

```bash
make all      # fmt + build + vet + test -race
```

That needs nothing installed but the Go toolchain, which is deliberate: you can run the whole of it before you have read anything or installed anything. [CI](.github/workflows/ci.yml) calls this same target rather than copying its commands, so neither can drift from the other. The target is shared; the environment is not. CI runs one platform, `ubuntu-latest`, at go.mod's Go version, and `test -race` needs a C toolchain, so a local failure CI would never have seen is possible in the other direction.

Five more checks run in CI and are Makefile targets too, kept out of `make all` because each costs a tool to install or a minute of wall clock:

```bash
make spdx         # every .go file carries its licence line; make spdx-fix adds them
make bench-build  # vet + test the bleve comparison, which is its own module
make lint         # golangci-lint, pinned to the version CI uses; covers bench/ too
make lint-docs    # markdownlint over every .md
make fuzz         # 30s each against the two segment-decoder fuzz targets
```

`bench-build` is separate from `make all` for a structural reason rather than a cost one: `bench/` is a nested module, so neither `go build ./...` nor `golangci-lint run ./...` at the root descends into it, and without a target naming it the bleve half of milestone 5's comparison would rot unnoticed between the runs that use it.

`make lint` and `make lint-docs` need `golangci-lint` and `markdownlint-cli2`; each target says so and names the install command rather than failing obscurely. `make fuzz` needs nothing but time, and it is the one most likely to find something no test covers — [SECURITY.md](SECURITY.md) names the segment decoder as the first place a hostile file lands. There is an optional [pre-commit config](.pre-commit-config.yaml) that runs the first three; nothing requires it, and CI does not use it.

## What not to break

Two assertions are decided by tests rather than by a reviewer:

- Fusion is invariant to scorer count.
- Neither `engine/` nor `fusion/` may import a scorer package.

The second holds for every scorer automatically, yours included: it reads the import graph, so it needs no baseline and no new test. Note that it covers `engine/` as well as `fusion/`, and that `engine` is asserted to import no weft package at all.

The first does not generalize by itself. `TestAddingAFourthScorerDoesNotChangeTheCallShape` and `TestAnyNumberOfScorersFuses` name the four scorers in the tree, so nothing runs yours until you add it to those two slices in `pkg/engine/architecture_test.go`. Add it there.

They are ordinary tests in `pkg/engine`, so `make all` — and therefore CI — already runs them. `make arch` runs them by name and verbosely, along with two you will meet only by tripping: `TestNoExternalDependencies`, and two golden-file tests. `TestEngineAPISurfaceIsUnchanged` fails when `engine`'s exported surface changes and asks you to record that cost in `docs/FINDINGS.md`; `TestPublicAPISurfaceIsUnchanged` does the same for `fusion` and every scorer package, which a caller builds directly. Both refresh with `WEFT_UPDATE_GOLDEN=1`. The second discovers packages instead of listing them, so adding a scorer trips it — that is not a hurdle, it is the addition becoming visible. Both read the source rather than a build, so they answer for the platforms named in `apiContexts` — `linux/amd64`, `darwin/arm64`, `windows/amd64` — and a declaration some of those cannot see is recorded with the ones that can. A fifth platform is a line in that list. `make arch` drops `-race`, so it is a different question from `make all`, not a louder version of it.

The line-count figure is a measurement, not a budget you have to fit under — with one exception worth knowing before it bites you. `TestFourthScorerIsUnderOneHundredLines` measures `scorer/recency` and nothing else, so a fifth scorer twice that size still passes, and milestone 3's ANN scorer will not fit under 100 lines either. For `scorer/recency` itself it is a hard failure at 100, `recency.go` counts 99, and the count includes comments and blank lines: one added line there turns `make all` red. That is deliberate — the figure is a published claim about this project, so changing the file means changing the claim, and the pull request is where you say so. The one thing the count does *not* include is the SPDX header and the blank line under it, because `make spdx` puts those on every file in the repository and charging a scorer for a repository-wide licensing decision would measure the wrong thing. If your own scorer is much larger, the pull request is also where you say why.

## Adding a scorer

The main extension path is written down once, in [README](README.md#adding-a-scorer): implement `engine.Scorer` and nothing in `engine/` or `fusion/` should need to change. If your change does need to touch them, that is the interesting part of the pull request — lead with it.

## Pull requests

Fill in [the template](.github/PULL_REQUEST_TEMPLATE.md). Its five sections ask what changed and why, how you verified it, what deserves attention, and what might bite later; answering them is most of the review.

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
