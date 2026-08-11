# Evaluation datasets for the graph scorer

**Question:** is there a public, relevance-labelled dataset that can measure whether graph proximity improves nDCG? Milestone 4 does not exist without one.

**Answer: yes, milestone 4 is viable.** No single dataset works off the shelf. The combination has to be **human-made labels plus a link graph joined from a separate source**, for the reason in §1.

Milestone 2's scope is unaffected — see §5.

---

## 1. Disqualification criterion

The graph scorer converts hop distance from a seed into `1/(1+hops)`. **A dataset whose relevance labels were derived from the same link structure is unusable:** the scorer would be reproducing the label-generation rule, so a higher nDCG is guaranteed and means nothing.

This is the same failure mode as the seed echo in [FINDINGS §2.3](FINDINGS.md). There, one text vote was counted twice; here, the answer key would be visible during the exam.

---

## 2. Candidates assessed

| Dataset | Verdict | Reason |
|---|---|---|
| NFCorpus | ❌ | Labels derived from link distance — see below |
| SCIDOCS | ❌ | BEIR classifies its task as "Citation-Prediction"; labels are citations, co-citations, co-views |
| HotpotQA | ❌ as primary | Partially circular — see below |
| Cora, CiteSeer, PubMed, WikiCS | ❌ | Clean link graphs, but no queries and no qrels. Node-classification benchmarks, not retrieval collections; there is nothing to compute nDCG@10 over |
| ClueWeb09 + TREC Web Track | ⚠️ blocked | Satisfies every requirement, unobtainable — see below |
| TREC-COVID ⋈ Semantic Scholar | ✅ recommended | §3 |
| DBpedia-Entity v2 | ✅ fallback | §3 |

### NFCorpus — the dangerous near miss

3,633 documents and 323 queries make it the first candidate anyone reaches for, because it fits an in-memory engine exactly. Its label construction:

| Grade | Rule |
|---|---|
| Highest | the NutritionFacts article (query) **links directly** to the medical document |
| Middle | the query links another NutritionFacts article which links the document (**two hops**) |
| Lowest | connected through the site's tag and topic system |

That is very nearly the function the graph scorer computes, and all 169,756 judgments in the original release are automatically extracted rather than human-assessed. Recorded explicitly because the attractive size makes it likely to be picked up again later.

### HotpotQA — usable only as an upper bound

The labels themselves are human: crowd workers wrote the questions and marked supporting sentences. The corpus construction is the problem. The authors built a hyperlink graph over Wikipedia lead paragraphs, then **showed annotators paragraph pairs connected by an edge** and asked for questions requiring both. Gold document pairs are therefore hyperlink edges by construction, and the entire query distribution is link-traversable.

Not useless: it bounds how well graph proximity can possibly do. No improvement here means the implementation is broken.

### ClueWeb09 — the right dataset, out of reach

- 70,575 human graded judgments over 200 topics on a 4-point scale (TREC Web Tracks 2009–2012)
- A complete web graph: 454,075,638 outlinks, 3 GB uncompressed, complete for Category B (50.2M English pages) as well as the full set
- Labels and graph are independent — NIST assessors did not judge by looking at links

Blocked by a required corpus licence agreement and 5 TB compressed. The qrels are freely available from trec.nist.gov; the document collection is gated. Recorded because it is the end goal once scale allows.

---

## 3. Recommendation — TREC-COVID joined with Semantic Scholar citations

Join the two sources on document id. **The point is that labels and graph come from different places.**

| Axis | Source | Independence |
|---|---|---|
| Labels | TREC-COVID qrels — biomedical experts and NIST assessors | not produced by looking at links ✓ |
| Graph | Semantic Scholar `citations` dataset (monthly snapshots, Datasets API) | played no part in label generation ✓ |

Why TREC-COVID specifically:

- **Judgment depth: 493.5 qrels per query on average** — the highest in BEIR, where most datasets are under 5.
- That depth is decisive for this measurement. Any document absent from qrels counts as irrelevant, so shallow judgments penalize a system for surfacing relevant-but-unjudged documents. The graph scorer exists to surface documents text cannot find, so it walks straight into that trap. 493.5 largely removes the risk.
- 171K documents and 50 queries — within reach of an in-memory index.
- nDCG@10 is BEIR's primary metric, matching milestone 4's metric directly.

**Fallback: DBpedia-Entity v2.** 467 queries, 49,280 query-entity judgments on a 3-point scale, crowdsourced with expert adjudication on disagreement. Labels are human and the graph is DBpedia's RDF structure, so independence holds. Costs more setup: 4.6M entities and a separate DBpedia dump to process.

---

## 4. Measurement requirements

### 4.1 Include the neighbourhood when restricting to the judged pool

Evaluating over judged documents only, as re-ranking, is standard practice and sufficient for an A/B. But **keeping only the pool turns every citation edge to an unjudged document into a dangling edge, which disables the graph scorer.**

Index the pool plus its one- and two-hop neighbours. Neighbours participate in traversal and count as unjudged when scoring.

### 4.2 Three arms, not two

Required by [FINDINGS §2.3](FINDINGS.md):

| Configuration | Purpose |
|---|---|
| text + vector | baseline |
| text + vector + `graph.New` | the graph scorer's real contribution, seeds excluded |
| text + vector + `graph.NewIncludingSeeds` | the seed-echo behaviour, quantifying how much double counting inflated it |

Without the third arm, an improvement still cannot be attributed to the graph rather than to doubled text weight.

### 4.3 Do not expect link structure from the tooling

In the `ir_datasets` catalogue, `links` and `citations` mean documentation links and bibliographic entries, not inter-document edges. Its entity types are docs, queries, qrels, docpairs and scoreddocs; **no dataset ships document-to-document edges.** BEIR's `corpus.jsonl` likewise carries only `_id`, `title` and `text`. The graph has to be joined in.

### 4.4 Sweep RRF `k` alongside

Damping is stronger than expected ([FINDINGS §3.2](FINDINGS.md)) — recency changed scores without changing order. Measuring the graph contribution at a fixed `k` risks measuring `k = 60` instead.

---

## 5. Effect on milestone 2

This survey ran ahead of milestone 2 because its outcome could have changed that scope. It does not, and it validates two existing design points:

- **Keep `Document.Links` keyed by document key.** The recommended path is exactly "join an external citation graph by document id", and many citation targets fall outside the corpus, remaining dangling. weft already handles that by design (`TestDanglingLinksAreIgnored`). A `DocID` adjacency list would have blocked this path.
- **Persist the graph as a first-class scorer.** There is no case for dropping it; milestone 4 is executable.

---

## Sources

- [BEIR corpus](https://huggingface.co/datasets/BeIR/beir-corpus) · [BEIR per-dataset statistics](https://www.elastic.co/search-labs/blog/evaluating-search-relevance-part-1)
- [NFCorpus](https://www.cl.uni-heidelberg.de/statnlpgroup/nfcorpus/) · [NFCorpus in ir_datasets](https://ir-datasets.com/nfcorpus.html) · [mteb/nfcorpus](https://huggingface.co/datasets/mteb/nfcorpus)
- [DBpedia-Entity v2](https://iai-group.github.io/DBpedia-Entity/) · [repository](https://github.com/iai-group/DBpedia-Entity) · [SIGIR'17 paper](https://dl.acm.org/doi/10.1145/3077136.3080751)
- [ClueWeb09](https://lemurproject.org/clueweb09.php/) · [TREC 2013 Web Track overview](https://trec.nist.gov/pubs/trec22/papers/WEB.OVERVIEW.pdf) · [TREC 2014 Web Track overview](https://trec.nist.gov/pubs/trec23/papers/overview-web.pdf)
- [HotpotQA (Yang et al., 2018)](https://nlp.stanford.edu/pubs/yang2018hotpotqa.pdf)
- [ir_datasets (MacAvaney et al., SIGIR 2021)](https://arxiv.org/pdf/2103.02280) · [Semantic Scholar Open Data Platform](https://arxiv.org/pdf/2301.10140)
