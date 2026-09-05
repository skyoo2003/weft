# Milestone 25 — test record

**Scope**: the OpenSearch query-DSL subset, `_bulk`, field mappings, two fuzz targets.
**Mechanical definition**: `pkg/` diff 0 lines. Held.

## What was written RED first, and what was not

RED-first: `TestEveryRowOfTheDSLTable`, `TestTheRefusalRateIsCounted`,
`TestAFilterNarrowsWithoutVoting`, `TestARequiredMatchIsNotSilentlyAnAnd`,
`TestBulkAppliesEveryActionAndReportsPerItem`, `TestBulkRefusesABatchItWouldReadOffByOne`,
`TestAMappingSurvivesAReopen`, `TestRemappingAFieldIsRefused`.

Not RED-first, and said so: the two fuzz targets. A fuzzer cannot be written to fail first
against a bug nobody has found; what they assert is a property (`parseSearch` returns a plan or a
refusal with a status ≥ 400 and a non-empty reason, and never a third thing).

## Two tests that had to be rewritten because they asserted the previous milestone

- `TestCreateIndexRefusesAMappingItCannotHonour` → `TestCreateIndexHonoursAMappingOrRefusesItByName`.
  Milestone 24 refused the whole `mappings` key; 25 honours five types and refuses the rest **by
  name**, and the index is removed again when the mapping is refused — a client told its mapping
  failed and then finding the index there would index by the wrong rule until something noticed.
- `TestAnUnsupportedQueryIsNotAnEmptyResult` lost the rows 25 implemented (`bool`, `term`,
  `prefix`, `range`, `from`) and gained the parts of each that stay refused. The table did not
  shrink; it moved.

## Findings the tests bought

1. **`bool.must` over a multi-token match silently narrows to `and`.** Found by
   `TestARequiredMatchIsNotSilentlyAnAnd`, written from the PRD row rather than from the code.
   `query.Must` intersects positions; a two-token match is two positions. The fix is `anyOf`
   (D-028) and it is in the server, not in `pkg/`.
2. **A weight of 0 does not silence a filter, it deletes the document.** The PRD registered this
   in the `bool.filter` row before any code existed and it was still the first thing tried;
   `TestAFilterNarrowsWithoutVoting` is what caught it, by asserting on the *order* of the two
   surviving documents rather than only on the set.
3. **A pattern over `query.MaxTerms` returns a truncated match with a 200 on it.** `Glob` caps
   the vocabulary scan and cannot report that it did. Inside a Go program the caller chose the
   pattern; over HTTP it is a partial answer presented as a complete one. Asking for
   `MaxTerms + 1` terms is what makes the truncation visible, and costs one vocabulary entry.

## Numbers

| check | result |
| --- | --- |
| package tests | 100% pass, coverage 83.1% → **87.4%** |
| refusal rate (published) | **8 of 25 rows = 32.0%** |
| `FuzzParseDSL` | 5,806,595 execs / 45s, 0 crashes, 332 interesting |
| `FuzzParseBulk` | 3,655,276 execs / 30s, 0 crashes, 347 interesting |
| `make compat` (`opensearch-py` 3.2.0) | 39 checks pass |
| `pkg/` diff | 0 lines |
| golden API files | byte-identical |
