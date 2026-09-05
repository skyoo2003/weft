# SPDX-License-Identifier: Apache-2.0
"""Milestone 24's judgment sentence, run rather than asserted.

    opensearch-py indexes, gets and searches against weftd with no modification.

Everything else in this milestone is a Go test against an httptest server, which
proves the handler does what the handler was written to do. Only a real client
can say whether the *protocol* is right — whether the fields opensearch-py reads
are the fields weftd sends, and whether the version handshake in D-026 actually
buys what it was claimed to buy.

Run through `make compat`, which skips when python or the client is absent so
that `make all` still needs nothing but the Go toolchain.
"""

import json
import shutil
import socket
import subprocess
import sys
import tempfile
import time

from opensearchpy import OpenSearch, helpers
from opensearchpy.exceptions import TransportError

INDEX = "papers"

# Milestone 25 gave these a mapping, and the mapping is the whole reason a range
# query works: an integer is indexed as query.EncodeInt's fixed-width hex, and
# only something that knows the field is a number can write the same encoding into
# the bound. PRD section 5 is the argument.
MAPPINGS = {
    "properties": {
        "title": {"type": "text"},
        "status": {"type": "keyword"},
        "views": {"type": "integer"},
        # recency is weft's, the way dimension is the k-NN plugin's: something has
        # to say which date is engine.Document.Time, and a mapping is the only
        # thing that knows a field's type at both index and query time.
        "published": {"type": "date", "recency": True},
        "vec": {"type": "knn_vector", "dimension": 3},
    }
}

DOCS = {
    "rrf": {"text": "reciprocal rank fusion combines rankings", "title": "rrf",
            "status": "published", "views": 42, "published": "2024-03-01", "vec": [1, 0, 0]},
    "bm25": {"text": "bm25 ranks documents by term frequency", "title": "bm25",
             "status": "published", "views": 7, "published": "2023-01-15", "vec": [0.9, 0.1, 0]},
    "pottery": {"text": "an unrelated document about pottery", "title": "pots",
                "status": "draft", "views": 1, "published": "2022-06-30", "vec": [0, 1, 0]},
}

# Indexed through the client's own bulk helper rather than one at a time, because
# what _bulk has to survive is opensearch-py's framing of NDJSON and not this
# script's.
BULK_DOCS = {
    "vectors": {"text": "dense vector retrieval combines with lexical search", "title": "vectors",
                "status": "draft", "views": 100, "published": "2025-11-11", "vec": [0, 0, 1]},
    "graphs": {"text": "graph proximity as a ranking signal", "title": "graphs",
               "status": "published", "views": 3, "published": "2021-02-02", "vec": [0, 0.1, 0.9]},
}


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def wait_for(port, proc, timeout=60.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise SystemExit(f"weftd exited before it listened (status {proc.returncode})")
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.5):
                return
        except OSError:
            time.sleep(0.2)
    raise SystemExit(f"weftd did not listen on {port} within {timeout}s")


def check(condition, message):
    if not condition:
        raise SystemExit(f"FAIL: {message}")
    print(f"  ok  {message}")


def main():
    port = free_port()
    data = tempfile.mkdtemp(prefix="weftd-compat-")
    # go run rather than a prebuilt binary: this target is meant to be runnable
    # from a fresh checkout with nothing installed but Go and the client.
    proc = subprocess.Popen(
        ["go", "run", "./cmd/weftd", "-addr", f"127.0.0.1:{port}", "-data", data],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    try:
        wait_for(port, proc)
        run(port)
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=15)
        except subprocess.TimeoutExpired:
            proc.kill()
        shutil.rmtree(data, ignore_errors=True)


def run(port):
    client = OpenSearch(hosts=[{"host": "127.0.0.1", "port": port}], use_ssl=False)

    print("handshake")
    info = client.info()
    check(info["version"]["distribution"] == "opensearch",
          f"the client reads distribution=opensearch (got {info['version']['distribution']!r})")
    check(info["version"]["number"] == "2.19.0",
          f"the client reads version 2.19.0 (got {info['version']['number']!r})")

    print("cluster")
    check(client.cluster.health()["status"] == "green", "cluster health is green")

    print("index lifecycle")
    client.indices.create(index=INDEX, body={"mappings": MAPPINGS})
    check(client.indices.exists(index=INDEX), "the index exists after create")
    mapping = client.indices.get_mapping(index=INDEX)
    check(mapping[INDEX]["mappings"]["properties"]["views"]["type"] == "integer",
          "the client reads back the mapping it sent")

    print("documents")
    for doc_id, body in DOCS.items():
        client.index(index=INDEX, id=doc_id, body=body, refresh=True)
    got = client.get(index=INDEX, id="rrf")
    check(got["found"] is True, "get reports found")
    check(got["_source"] == DOCS["rrf"],
          f"_source round-trips unchanged (got {json.dumps(got['_source'], sort_keys=True)})")

    print("bulk — the client's own helper, not this script's NDJSON")
    ok, errors = helpers.bulk(
        client,
        [{"_index": INDEX, "_id": doc_id, "_source": body} for doc_id, body in BULK_DOCS.items()],
        refresh=True,
    )
    check(ok == len(BULK_DOCS), f"the bulk helper reports {len(BULK_DOCS)} indexed (got {ok})")
    check(not errors, f"the bulk helper reports no errors (got {errors})")
    check(client.exists(index=INDEX, id="graphs"), "a bulk-indexed document is retrievable")

    print("search")
    res = client.search(index=INDEX, body={"query": {"match": {"text": "rank fusion"}}, "size": 10})
    ids = [h["_id"] for h in res["hits"]["hits"]]
    check(ids, f"match returned hits (got {ids})")
    check(ids[0] == "rrf", f"the document both token streams rank comes first (got {ids})")
    check("pottery" not in ids, f"a document holding neither term is absent (got {ids})")
    check(res["hits"]["hits"][0]["_source"] == DOCS["rrf"], "a hit carries the body that was indexed")

    everything = client.search(index=INDEX, body={"query": {"match_all": {}}, "size": 100})
    total = len(DOCS) + len(BULK_DOCS)
    check(everything["hits"]["total"]["value"] == total,
          f"match_all counts every document (got {everything['hits']['total']['value']}, want {total})")

    print("DSL — milestone 25")

    def ids_for(body):
        return sorted(h["_id"] for h in client.search(index=INDEX, body=body, size=100)["hits"]["hits"])

    check(ids_for({"query": {"term": {"status": "draft"}}}) == ["pottery", "vectors"],
          "term on a keyword field finds the drafts")
    check(ids_for({"query": {"range": {"views": {"gte": 7, "lte": 42}}}}) == ["bm25", "rrf"],
          "a range over a mapped integer is numeric and not lexicographic")
    check(ids_for({"query": {"range": {"published": {"gte": "2024-01-01"}}}}) == ["rrf", "vectors"],
          "a range over a mapped date reads the client's format")
    check(ids_for({"query": {"match_phrase": {"text": "reciprocal rank fusion"}}}) == ["rrf"],
          "match_phrase wants the words consecutive")
    check(ids_for({"query": {"prefix": {"text": "rank"}}}) == ["bm25", "graphs", "rrf"],
          "prefix matches the term's head")
    check(ids_for({"query": {"bool": {
              "must": [{"match": {"text": "ranking rankings"}}],
              "filter": [{"term": {"status": "published"}}]}}}) == ["graphs", "rrf"],
          "bool.must with a filter narrows without dropping the ranking")
    check(ids_for({"query": {"bool": {
              "should": [{"match": {"text": "pottery graph"}}],
              "must_not": [{"term": {"status": "draft"}}]}}}) == ["graphs"],
          "bool.must_not excludes what it names")

    page = client.search(index=INDEX, body={"query": {"match_all": {}}, "from": 2, "size": 2})
    check(len(page["hits"]["hits"]) == 2, "from and size return one page")

    print("hybrid — milestone 26, the only round that buys new evidence")
    check(ids_for({"query": {"knn": {"vec": {"vector": [1, 0, 0], "k": 2}}}, "size": 2}) == ["bm25", "rrf"],
          "knn is a stream: the nearest two vectors come back")
    fused = ids_for({"query": {"hybrid": {"queries": [
        {"match": {"text": "pottery"}},
        {"knn": {"vec": {"vector": [1, 0, 0], "k": 2}}}]}}, "size": 5})
    check("pottery" in fused and "rrf" in fused,
          f"hybrid fuses a text stream and a vector stream (got {fused})")

    def first(body):
        hits = client.search(index=INDEX, body=body, size=5)["hits"]["hits"]
        if not hits:
            raise SystemExit(f"FAIL: no hits for {body}")
        return hits[0]["_id"]

    def weighted(text_w, vector_w):
        return {"query": {"hybrid": {
            "queries": [{"match": {"text": "pottery"}},
                        {"knn": {"vec": {"vector": [1, 0, 0], "k": 3}}}],
            "weights": [text_w, vector_w]}}}

    check(first(weighted(10, 0.01)) == "pottery", "weighting the text stream puts the text match first")
    check(first(weighted(0.01, 10)) == "rrf", "weighting the vector stream puts the nearest vector first")

    # The fourth signal. Nothing about this clause is different from the three
    # above, which is milestone 26's entire claim.
    recent = ids_for({"query": {"function_score": {"gauss": {"published": {}}}}, "size": 2})
    check(recent[:1] != [] and "vectors" in recent,
          f"recency is a stream: the newest documents come back (got {recent})")
    three = ids_for({"query": {"hybrid": {"queries": [
        {"match": {"text": "ranking"}},
        {"knn": {"vec": {"vector": [0, 0.1, 0.9], "k": 3}}},
        {"function_score": {"gauss": {"published": {}}}}]}}, "size": 5})
    check(len(three) >= 3, f"three signals fuse in one query (got {three})")

    print("refusals — D-026")
    for name, body, want in [
        ("aggs", {"aggs": {"v": {"terms": {"field": "title"}}}}, 501),
        ("highlight", {"query": {"match": {"text": "fusion"}}, "highlight": {"fields": {"text": {}}}}, 501),
        ("sort", {"query": {"match": {"text": "fusion"}}, "sort": [{"views": "asc"}]}, 400),
        ("nested bool", {"query": {"bool": {"must": [
            {"bool": {"must": [{"match": {"text": "fusion"}}]}}]}}}, 400),
        ("minimum_should_match", {"query": {"bool": {
            "should": [{"match": {"text": "fusion"}}], "minimum_should_match": 1}}}, 400),
        ("ids", {"query": {"ids": {"values": ["rrf"]}}}, 501),
        ("numeric range on an unmapped field", {"query": {"range": {"nope": {"gte": 1}}}}, 400),
        ("decay parameters", {"query": {"function_score": {
            "gauss": {"published": {"origin": "now", "scale": "10d"}}}}}, 400),
        ("a search pipeline", {"query": {"hybrid": {
            "queries": [{"match": {"text": "fusion"}}], "search_pipeline": "norm"}}}, 400),
        ("script_score", {"query": {"script_score": {
            "query": {"match_all": {}}, "script": {"source": "1"}}}}, 400),
    ]:
        try:
            client.search(index=INDEX, body=body)
        except TransportError as e:
            check(e.status_code == want, f"{name} is refused with {e.status_code} (want {want})")
            reason = ""
            if isinstance(e.info, dict):
                reason = e.info.get("error", {}).get("reason", "")
            check(bool(reason), f"{name}'s refusal carries a reason: {reason[:70]}")
        else:
            raise SystemExit(f"FAIL: {name} returned success; it must be refused, not answered empty")

    # scroll is a route rather than a body, so it is checked through the client's
    # own scroll parameter: a 404 here would send a user looking for a typo.
    try:
        client.search(index=INDEX, body={"query": {"match_all": {}}}, scroll="1m")
    except TransportError as e:
        check(e.status_code == 501, f"scroll is refused with {e.status_code} (want 501)")
    else:
        raise SystemExit("FAIL: scroll returned success; it must be refused")

    print("teardown")
    client.delete(index=INDEX, id="pottery", refresh=True)
    check(not client.exists(index=INDEX, id="pottery"), "a deleted document is gone")
    client.indices.delete(index=INDEX)
    check(not client.indices.exists(index=INDEX), "the index is gone after delete")

    print("\nPASS: opensearch-py drove weftd unmodified.")


if __name__ == "__main__":
    sys.exit(main())
