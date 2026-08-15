# Cutting a release

Four steps, in this order. Everything before step 3 is reversible; step 3 is not.

This procedure used to live in [CONTRIBUTING](CONTRIBUTING.md) and now lives only here, so that there is one copy of it.

## 0. What a tag means

A commit you can name. Not a support promise, and not a claim that the code became production-ready — [README](README.md#status) says it is not, and a version number does not change what the code does.

The module version and `formatVersion` are independent, and [CHANGELOG](CHANGELOG.md) says why. The API can break inside a minor while this is v0.x.

## 1. Merge the documentation the tag will freeze

A tag points at a tree, so a correction written afterwards is not in it. That is the whole reason `docs/FINDINGS.md`'s line count was fixed before the first tag rather than after.

```bash
changie batch v0.1.0     # changes/unreleased/*.yaml -> changes/v0.1.0.md
make changelog           # regenerates CHANGELOG.md from every batched version
```

`changie batch` empties `changes/unreleased/`. If it produces nothing, there is nothing to release — write the entry first with `make changelog-new`.

In the same commit, fix whatever else the tag makes false. As of the first tag that is [README](README.md#status)'s "no tag yet" and [SECURITY](SECURITY.md)'s "no releases yet".

## 2. Confirm that exact commit is green

Name the SHA and let the command fail, rather than reading a list:

```bash
sha=$(git fetch origin main && git rev-parse FETCH_HEAD)
gh run watch "$(gh run list --workflow=ci.yml --commit "$sha" --json databaseId --jq '.[0].databaseId')" --exit-status
```

Every part of the first line is load-bearing. `origin/main` on its own is a local ref, only as fresh as your last fetch, and step 1 merged through GitHub — so without the fetch you can watch a green run and then permanently tag the commit from before your own release documentation landed. `--branch=main` answers a different question again: it returns whatever ran most recently on the branch, which need not be the commit you are about to tag, and it exits 0 no matter how that run concluded. `--commit` binds the answer to the tag; `--exit-status` turns a green run from something you read into something you cannot skip past.

An empty id means no run exists for that SHA yet. Wait for it rather than tagging.

## 3. Tag and push — the irreversible step

```bash
git tag -a v0.1.0 "$sha" -m "v0.1.0"
git push origin v0.1.0
```

Once the Go module proxy has served a version, deleting the tag does not withdraw it: the version stays resolvable, pointing at whatever it pointed at. There is no undo. The remedy for a bad tag is the next tag.

Push the tag yourself rather than letting tooling invent one — anything that creates a tag for you creates it at the default branch's HEAD, which need not be the commit you just checked.

## 4. Watch the automation

The push triggers two workflows, and neither can prevent anything, because both start after the tag exists:

- [`ci.yml`](.github/workflows/ci.yml) runs the full gate against the tagged tree. Redundant when the tag is on a commit main already proved green, which is exactly why step 2 is not optional.
- [`release.yml`](.github/workflows/release.yml) refuses outright if `changes/<tag>.md` is missing, then runs GoReleaser: cross-compiled archives of `cmd/weft`, `checksums.txt`, an SBOM per archive, and the GitHub release itself with `changes/<tag>.md` as its notes.

```bash
gh run list --workflow=release.yml --limit 1
gh release view v0.1.0
GOPROXY=proxy.golang.org go list -m github.com/skyoo2003/weft@v0.1.0
```

The last line is the one that matters for a library: it is what a user's `go get` will do.

## What is deliberately not automated

**Choosing the version number.** Nothing here infers a bump from commit messages. weft is v0.x, the API may break in a minor, and a machine reading `feat:` prefixes would be guessing at a decision that takes ten seconds to make deliberately.

**Docker images and a Homebrew tap.** Both are distribution channels with a maintenance schedule this project cannot promise, and neither is on the path of anyone deciding whether to depend on the library. [`.goreleaser.yaml`](.goreleaser.yaml) says the same at the top.

**Retractions.** No version has needed one. If one does, `retract` goes in `go.mod`, and the rule to remember is that the go command reads retractions only from the highest published version — so a later release whose `go.mod` dropped the block would silently un-retract. Add the CI check that asserts it at the same time as the first retraction, not before.
