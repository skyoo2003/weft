# Support

One maintainer. Replies are slow but real, and nothing here carries a response time — a promise this project cannot keep is worse than no promise.

## Where to take what

| You have | Take it to |
| --- | --- |
| A bug — wrong results, a panic, a corrupt index | [Bug report](https://github.com/skyoo2003/weft/issues/new?template=bug.yml). It asks for a commit SHA because there is no released version to name yet |
| A change you want to build | [Proposal](https://github.com/skyoo2003/weft/issues/new?template=proposal.yml), before writing it. Not for permission — to find out whether it is already recorded as rejected in [DECISIONS](docs/DECISIONS.md) or as unverified in [FINDINGS](docs/FINDINGS.md) |
| A question | A blank issue. No form, no template |
| A vulnerability | **Not the issue tracker.** [SECURITY.md](SECURITY.md) |
| A patch | [CONTRIBUTING.md](CONTRIBUTING.md) |

## What is not here

No Discord, no Slack, no mailing list. A channel with nobody in it reads as an abandoned project, which is worse than not having one — so there is one queue, the issue tracker, and it is the one the maintainer actually reads.

No commercial support and no consulting.

## Before you file

weft is a library that is [explicitly not production software](README.md#status). Three of the most common surprises are documented rather than broken:

- **Sustained query load collapses rather than degrades.** At its own sequential rate p50 goes 39 ms to 1.27 s and queries are shed. That is measured and documented, not a bug.
- **A commit blocks reads for as long as it takes,** 11 s for a 20,000-document batch, and nothing bounds the window.
- **Documents cannot be deleted.** Tombstones and DocID namespacing are one design problem and neither is built.

The [Limitations table](README.md#limitations) has the rest. Something on that list is not worth an issue unless you can say what it should do instead.

## What a `priority:` label means

That the maintainer read the issue and formed an opinion about where it sits in a queue. It is not a commitment about when anything ships, and no label implies a date.
