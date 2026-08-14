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

They are ordinary tests in `pkg/engine`, so `make all` — and therefore CI — already runs them. `make arch` runs them by name and verbosely, along with two you will meet only by tripping: `TestNoExternalDependencies`, and two golden-file tests. `TestEngineAPISurfaceIsUnchanged` fails when `engine`'s exported surface changes and asks you to record that cost in `docs/FINDINGS.md`; `TestPublicAPISurfaceIsUnchanged` does the same for `fusion` and every scorer package, which a caller builds directly. Both refresh with `WEFT_UPDATE_GOLDEN=1`. The second discovers packages instead of listing them, so adding a scorer trips it — that is not a hurdle, it is the addition becoming visible. `make arch` drops `-race`, so it is a different question from `make all`, not a louder version of it.

The line-count figure is a measurement, not a budget you have to fit under — with one exception worth knowing before it bites you. `TestFourthScorerIsUnderOneHundredLines` measures `scorer/recency` and nothing else, so a fifth scorer twice that size still passes, and milestone 3's ANN scorer will not fit under 100 lines either. For `scorer/recency` itself it is a hard failure at 100, `recency.go` sits at 99, and the count includes comments and blank lines: one added line there turns `make all` red. That is deliberate — 99 is a published claim about this project, so changing the file means changing the claim, and the pull request is where you say so. If your own scorer is much larger, the pull request is also where you say why.

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

Four steps, in this order. Everything before the push is reversible; the push is not.

1. **Merge the documentation the tag will freeze.** A tag points at a tree, so a correction written afterwards is not in it — the whole reason `docs/FINDINGS.md`'s line count was fixed before the first tag rather than after. That means: a [CHANGELOG](CHANGELOG.md) entry **only if the release moved one of three things** — either golden file under `pkg/engine/testdata/`, `formatVersion` in `pkg/engine/segment.go`, or go.mod's Go version, each a file a test or the toolchain already watches. If none moved, write nothing; the silence is the claim that there is nothing for a caller to do. Either way the heading stops saying `unreleased`, and whatever else the tag makes false goes in the same commit — [README](README.md#status)'s "no tag yet", [SECURITY.md](SECURITY.md)'s "no releases yet".
2. **Confirm that exact commit is green** — name the SHA and let the command fail, rather than reading a list:
   ```bash
   sha=$(git fetch origin main && git rev-parse FETCH_HEAD)
   gh run watch "$(gh run list --workflow=ci.yml --commit "$sha" --json databaseId --jq '.[0].databaseId')" --exit-status
   ```
   Every part of that first line is load-bearing. `origin/main` on its own is a local ref that is only as fresh as your last fetch, and step 1 merged through GitHub — so without the fetch you can watch a green run and then permanently tag the commit from before your own release documentation landed. `--branch=main` answers a different question again: it returns whatever ran most recently on the branch, which need not be the commit you are about to tag, and it exits 0 no matter how that run concluded. `--commit` binds the answer to the tag, `--exit-status` turns a green run from something you read into something you cannot skip past. An empty id means no run exists for that SHA yet — wait for it rather than tagging.
3. `git tag -a <tag> "$sha"`, then `git push origin <tag>`. Push the tag yourself instead of letting step 4 invent one: `gh release create` on a tag that does not exist yet creates it at the default branch's HEAD, which need not be the commit you just checked. Once the Go module proxy has served a version, deleting the tag does not withdraw it — the version stays resolvable, pointing at whatever it pointed at. `v[0-9]*` tags do trigger [CI](.github/workflows/ci.yml), but that run starts after the push: it reports a bad tag, it cannot stop one, and the remedy is the next tag rather than a deletion.
4. `gh release create <tag> --verify-tag --generate-notes` — the notes come from the merged pull requests, so nothing is retyped. `--verify-tag` aborts when the tag is not already on the remote, which makes the trap in step 3 a check rather than something to remember.
