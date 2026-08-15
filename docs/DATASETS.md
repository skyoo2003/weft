# Evaluation datasets for the graph scorer

**Question:** is there a public, relevance-labelled dataset that can measure whether graph proximity improves nDCG? Milestone 4 does not exist without one.

**Answer: yes.** No single dataset works off the shelf — it has to be **human-made labels plus a link graph joined from a separate source** (§1). Milestone 2's scope is unaffected (§4).

---

## 1. Disqualification criterion

The graph scorer converts hop distance from a seed into `1/(1+hops)`. **A dataset whose labels were derived from the same link structure is unusable:** the scorer reproduces the label-generation rule, so a higher nDCG is guaranteed and means nothing. Same failure mode as the seed echo in [FINDINGS §2.3](FINDINGS.md) — there one text vote was counted twice, here the answer key would be visible during the exam.

---

## 2. Candidates assessed

| Dataset | Verdict | Reason |
| --- | --- | --- |
| NFCorpus | ❌ | Labels derived from link distance — see below |
| SCIDOCS | ❌ | BEIR classifies its task as "Citation-Prediction"; labels are citations, co-citations, co-views |
| Cora, CiteSeer, PubMed, WikiCS | ❌ | Clean link graphs but no queries and no qrels. Node-classification benchmarks, so there is nothing to compute nDCG@10 over |
| HotpotQA | ❌ as primary | Labels are human, but the authors built a Wikipedia hyperlink graph and showed annotators **link-connected paragraph pairs** to write questions from. Gold pairs are hyperlink edges by construction. Usable only as an upper bound: no improvement here means the implementation is broken |
| ClueWeb09 + TREC Web Track | ⚠️ blocked | Satisfies everything — 70,575 human graded judgments over 200 topics, plus a complete web graph (454M outlinks, 3 GB uncompressed) independent of the labels. Blocked by a required corpus licence and 5 TB compressed. qrels are free from trec.nist.gov; the collection is gated. The end goal once scale allows |
| **TREC-COVID ⋈ Semantic Scholar** | ✅ recommended | §3 |
| DBpedia-Entity v2 | ✅ fallback | 467 queries, 49,280 judgments on a 3-point scale, crowdsourced with expert adjudication. Labels human, graph is DBpedia's RDF structure. Costs more setup: 4.6M entities and a separate dump to process |

### NFCorpus — the dangerous near miss

3,633 documents and 323 queries make it the first candidate anyone reaches for, because it fits an in-memory engine exactly. Its label construction:

| Grade | Rule |
| --- | --- |
| Highest | the NutritionFacts article (query) **links directly** to the medical document |
| Middle | the query links another article which links the document (**two hops**) |
| Lowest | connected through the site's tag and topic system |

That is very nearly the function the graph scorer computes, and all 169,756 judgments in the original release are automatically extracted rather than human-assessed. Recorded explicitly because the attractive size makes it likely to be picked up again.

---

## 3. Recommendation — TREC-COVID joined with Semantic Scholar citations

Join on document id. **The point is that labels and graph come from different places.**

| Axis | Source | Independence |
| --- | --- | --- |
| Labels | TREC-COVID qrels — biomedical experts and NIST assessors | not produced by looking at links ✓ |
| Graph | Semantic Scholar `citations` dataset (monthly snapshots, Datasets API) | played no part in label generation ✓ |

Why TREC-COVID specifically:

- **493.5 qrels per query on average** — the highest in BEIR, where most are under 5.
- That depth is decisive here. Documents absent from qrels count as irrelevant, so shallow judgments penalize a system for surfacing relevant-but-unjudged documents — exactly what the graph scorer exists to do.
- 171K documents and 50 queries, within reach of an in-memory index.
- nDCG@10 is BEIR's primary metric, matching milestone 4's directly.

### Measurement requirements

1. **Include the neighbourhood when restricting to the judged pool.** Re-ranking over judged documents only is standard and sufficient for an A/B, but keeping only the pool turns every citation edge to an unjudged document into a dangling edge, disabling the graph scorer. Index the pool plus its one- and two-hop neighbours; neighbours participate in traversal and count as unjudged when scoring.
2. **Three arms, not two** — required by [FINDINGS §2.3](FINDINGS.md): `text + vector` as baseline, `+ graph.New` for the real contribution, `+ graph.NewIncludingSeeds` to quantify how much double counting inflated it. Without the third, an improvement cannot be attributed to the graph rather than to doubled text weight.
3. **Expect no link structure from the tooling.** In the `ir_datasets` catalogue, `links` and `citations` mean documentation links and bibliographic entries, not inter-document edges; its entity types are docs, queries, qrels, docpairs and scoreddocs. BEIR's `corpus.jsonl` carries only `_id`, `title` and `text`. The graph must be joined in.
4. **Sweep RRF `k` alongside.** Damping is stronger than expected ([FINDINGS §3.2](FINDINGS.md)), so measuring the graph contribution at a fixed `k` risks measuring `k = 60` instead.

---

## 4. Effect on milestone 2

This survey ran ahead of milestone 2 because its outcome could have changed that scope. It does not, and it validates two existing design points:

- **Keep `Document.Links` keyed by document key.** The recommended path is exactly "join an external citation graph by document id", and many targets fall outside the corpus, remaining dangling — already handled by design (`TestDanglingLinksAreIgnored`). A `DocID` adjacency list would have blocked this path.
- **Persist the graph as a first-class scorer.** No case for dropping it; milestone 4 is executable.

---

## Sources

[BEIR corpus](https://huggingface.co/datasets/BeIR/beir-corpus) · [BEIR statistics](https://www.elastic.co/search-labs/blog/evaluating-search-relevance-part-1) · [NFCorpus](https://www.cl.uni-heidelberg.de/statnlpgroup/nfcorpus/) · [NFCorpus in ir_datasets](https://ir-datasets.com/nfcorpus.html) · [mteb/nfcorpus](https://huggingface.co/datasets/mteb/nfcorpus) · [DBpedia-Entity v2](https://iai-group.github.io/DBpedia-Entity/) · [SIGIR'17 paper](https://dl.acm.org/doi/10.1145/3077136.3080751) · [ClueWeb09](https://lemurproject.org/clueweb09.php/) · [TREC 2013 Web Track](https://trec.nist.gov/pubs/trec22/papers/WEB.OVERVIEW.pdf) · [TREC 2014 Web Track](https://trec.nist.gov/pubs/trec23/papers/overview-web.pdf) · [HotpotQA](https://nlp.stanford.edu/pubs/yang2018hotpotqa.pdf) · [ir_datasets](https://arxiv.org/pdf/2103.02280) · [Semantic Scholar Open Data](https://arxiv.org/pdf/2301.10140)
