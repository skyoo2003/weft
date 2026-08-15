# Governance

One maintainer, [@skyoo2003](https://github.com/skyoo2003), who decides everything. This document exists to say that plainly, and to say what would have to change for it to stop being true — not to describe a committee that does not exist.

Writing a voting procedure for a project with one contributor would be describing a process nobody can run. The interesting content of governance here is the opposite: which decisions are already constrained, and by what.

## What is not up to the maintainer

Three things overrule an opinion, including the maintainer's, and each is enforced by something other than agreement:

| Constraint | Enforced by |
| --- | --- |
| Fusion may not know that any scorer exists | `TestNeitherEngineNorFusionImportsAScorer` reads the import graph |
| The public API cannot change silently | Golden files under `pkg/engine/testdata/`, which fail the build on any diff |
| The module has no external dependencies | `TestNoExternalDependencies` |

A change that contradicts one of these does not need a maintainer to reject it; the gate rejects it first. Moving the constraint is a separate, deliberate act — and if the constraint is wrong, that argument belongs in the pull request that moves it.

Two documents carry decisions rather than code. [`docs/DECISIONS.md`](docs/DECISIONS.md) records what was expensive to reverse and why; [`docs/FINDINGS.md`](docs/FINDINGS.md) records what has actually been measured, including what has not. A change that contradicts either makes that document part of the change. This is the closest thing here to a governing rule: **claims are kept separate from measurements, and neither is quietly edited to match the other.**

## How decisions get made now

The maintainer decides, in the open, in the issue or pull request. Anything expensive to reverse gets a `DECISIONS.md` entry stating the alternatives and why they lost, so that a future disagreement starts from what was already considered rather than from scratch.

Nothing is decided in private, because there is no private channel — [SUPPORT.md](SUPPORT.md) explains why there is no chat room.

## What would change this

**A second maintainer.** That is the trigger, and it is the only one. At that point this document is rewritten to say who owns which paths, how the two of them break a tie, and how a third would be added. [`.github/CODEOWNERS`](.github/CODEOWNERS) already carries the path split, currently pointing everywhere at one name — that file is the skeleton this section would fill in.

Until then, adding a governance board, a technical committee or a vote would add decision cost without resolving any dispute, because there is nobody to dispute with.

**A corporate contributor** would raise the question of a CLA or DCO. There is deliberately neither today ([CONTRIBUTING](CONTRIBUTING.md#pull-requests)): contributions are under [Apache-2.0](LICENSE), the licence already on this repository, and adding a signing step in front of the first external pull request would be friction paid before any benefit.

## Succession

There is none, and that is a real risk rather than an oversight. If the maintainer stops, the project stops. What survives is the licence — Apache-2.0 lets anyone fork and continue without asking — and the fact that everything needed to do so is in the repository: no build secrets, no private infrastructure, no dependency on an account other than the GitHub one. The [release procedure](RELEASE.md) is written down for the same reason.
