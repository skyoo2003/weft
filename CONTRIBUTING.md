# Contributing

One maintainer. Replies are slow but real.

Behavior in this repository is covered by the [Code of Conduct](CODE_OF_CONDUCT.md). Vulnerabilities go to [SECURITY.md](SECURITY.md), not to the issue tracker.

## The gate

```bash
make all      # fmt + build + vet + test -race
```

That is the whole local check, and it is exactly what [CI](.github/workflows/ci.yml) runs — CI calls this target rather than copying its commands, so neither can drift from the other. The target is shared; the environment is not. CI runs one platform, `ubuntu-latest`, at go.mod's Go version, and `test -race` needs a C toolchain, so a local failure CI would never have seen is possible in the other direction.

## What not to break

Two assertions are decided by tests rather than by a reviewer:

- Fusion is invariant to scorer count.
- Neither `engine/` nor `fusion/` may import a scorer package.

The second holds for every scorer automatically, yours included: it reads the import graph, so it needs no baseline and no new test. Note that it covers `engine/` as well as `fusion/`, and that `engine` is asserted to import no weft package at all.

The first does not generalize by itself. `TestAddingAFourthScorerDoesNotChangeTheCallShape` and `TestAnyNumberOfScorersFuses` name the four scorers in the tree, so nothing runs yours until you add it to those two slices in `pkg/engine/architecture_test.go`. Add it there.

They are ordinary tests in `pkg/engine`, so `make all` — and therefore CI — already runs them. `make arch` runs them by name and verbosely, along with two you will meet only by tripping: `TestNoExternalDependencies`, and `TestEngineAPISurfaceIsUnchanged`, which fails when `engine`'s exported surface changes and asks you to record that cost in `docs/FINDINGS.md` before refreshing the golden file with `WEFT_UPDATE_GOLDEN=1`. `make arch` drops `-race`, so it is a different question from `make all`, not a louder version of it.

The line-count figure is a measurement, not a budget you have to fit under — with one exception worth knowing before it bites you. `TestFourthScorerIsUnderOneHundredLines` measures `scorer/recency` and nothing else, so a fifth scorer twice that size still passes, and milestone 3's ANN scorer will not fit under 100 lines either. For `scorer/recency` itself it is a hard failure at 100, `recency.go` sits at 99, and the count includes comments and blank lines: one added line there turns `make all` red. That is deliberate — 99 is a published claim about this project, so changing the file means changing the claim, and the pull request is where you say so. If your own scorer is much larger, the pull request is also where you say why.

## Adding a scorer

The main extension path is written down once, in [README](README.md#adding-a-scorer): implement `engine.Scorer` and nothing in `engine/` or `fusion/` should need to change. If your change does need to touch them, that is the interesting part of the pull request — lead with it.

## Pull requests

Fill in [the template](.github/PULL_REQUEST_TEMPLATE.md). Its five sections ask what changed and why, how you verified it, what deserves attention, and what might bite later; answering them is most of the review.

Commit messages: say why. `git log -p` already says what.

No CLA and no DCO sign-off. Contributions are under [Apache-2.0](LICENSE), the license already on this repository.

## Before a large change

Open an issue first. Not for permission — to find out whether the thing you are about to build is already recorded as rejected in [docs/DECISIONS.md](docs/DECISIONS.md) or as unverified in [docs/FINDINGS.md](docs/FINDINGS.md).

Neither document repeats anything readable from the code. If a change contradicts one of them, that document is part of the change.

## Stability

The API can break while this is v0.x. Changes that are expensive to reverse get a `DECISIONS.md` entry; everything else can move without ceremony.
