---
title: weft
---

A search engine where ranking signals are interchangeable. Go, from scratch, standard library only.

This site is the `docs/` directory of [the repository](https://github.com/skyoo2003/weft), rendered. Nothing here is written for the site — these are the same files you get when you clone, and the README is deliberately not duplicated onto this page.

**weft is not usable in production.** Deletion reclaims nothing, a commit holds a write lock for as long as it takes with reads queueing behind it, and sustained query load collapses rather than degrading. [STATUS](docs/status/) carries where the project is and [LIMITATIONS](docs/limitations/) the full list.

## The documents

Each answers one question, and only that one.

### Where it is

- [STATUS](docs/status/) — every milestone's state, the published nDCG table, and the debts not yet paid
- [LIMITATIONS](docs/limitations/) — what weft does not do, grouped by what it costs you
- [FINDINGS](docs/findings/) — what the milestones actually proved, what they cost, and what is still open

### How it is built

- [ARCHITECTURE](docs/architecture/) — the layout, which way dependencies point, and the three assertions a test decides
- [SCORERS](docs/scorers/) — how to plug your own ranking signal in
- [FORMAT](docs/format/) — the on-disk format, version 5; v2 through v5 all read
- [DECISIONS](docs/decisions/) — decisions expensive to reverse, and why

### How it was measured

- [EVAL](docs/eval/) — how milestone 4's numbers were produced, and why to doubt them
- [DATASETS](docs/datasets/) — the evaluation dataset survey behind milestone 4
- [PERF](docs/perf/) — how milestone 5's latency numbers are produced, and the load-point rule
- [ADOPTION](docs/adoption/) — how milestone 6 tested whether the documentation is enough
- [RESEARCH](docs/research/) — one round of community and competitive research

## Elsewhere

- [Repository](https://github.com/skyoo2003/weft)
- [Contributing](https://github.com/skyoo2003/weft/blob/main/CONTRIBUTING.md)
- [Security policy](https://github.com/skyoo2003/weft/blob/main/SECURITY.md)
- [Changelog](https://github.com/skyoo2003/weft/blob/main/CHANGELOG.md)
