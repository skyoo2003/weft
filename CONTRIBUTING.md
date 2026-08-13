# Contributing

One maintainer. Replies are slow but real.

## The gate

```bash
make all      # fmt + build + vet + test -race
```

That is the whole local check, and it is exactly what [CI](.github/workflows/ci.yml) runs — CI calls this target rather than copying its commands, so a local pass and a CI pass cannot mean different things.

## What not to break

Two assertions hold for every scorer, the existing ones and yours, and tests decide them rather than a reviewer:

- Fusion is invariant to scorer count.
- `fusion/` cannot see any scorer package.

They are ordinary tests in `pkg/engine`, so `make all` — and therefore CI — already runs them. `make arch` reruns that same set by name, verbosely, for when you want to read them one at a time. Break one and CI says so before a person does.

The third figure is a measurement, not a gate. The fourth scorer cost 99 implementation lines against a 100-line budget, and `TestFourthScorerIsUnderOneHundredLines` measures `scorer/recency` alone — a fifth scorer twice that size still passes. Read it as what this project claims about itself, not as a limit you have to fit under; milestone 3's ANN scorer will not fit under it either. If yours is much larger, the pull request is the place to say why.

What these mean and why: [README — Adding a scorer](README.md#adding-a-scorer).

## Adding a scorer

The main extension path is written down once, in [README](README.md#adding-a-scorer): implement `engine.Scorer` and nothing in `engine/` or `fusion/` should need to change. If your change does need to touch them, that is the interesting part of the pull request — lead with it.

## Pull requests

Fill in [the template](.github/PULL_REQUEST_TEMPLATE.md). It asks what changed, how you verified it, and what deserves attention; answering those three is most of the review.

Commit messages: say why. `git log -p` already says what.

No CLA and no DCO sign-off. Contributions are under [Apache-2.0](LICENSE), the license already on this repository.

## Before a large change

Open an issue first. Not for permission — to find out whether the thing you are about to build is already recorded as rejected in [docs/DECISIONS.md](docs/DECISIONS.md) or as unverified in [docs/FINDINGS.md](docs/FINDINGS.md).

Neither document repeats anything readable from the code. If a change contradicts one of them, that document is part of the change.

## Stability

The API can break while this is v0.x. Changes that are expensive to reverse get a `DECISIONS.md` entry; everything else can move without ceremony.
