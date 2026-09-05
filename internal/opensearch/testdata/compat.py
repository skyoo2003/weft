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

from opensearchpy import OpenSearch
from opensearchpy.exceptions import TransportError

INDEX = "papers"
DOCS = {
    "rrf": {"text": "reciprocal rank fusion combines rankings", "title": "rrf", "views": 42},
    "bm25": {"text": "bm25 ranks documents by term frequency", "title": "bm25", "views": 7},
    "pottery": {"text": "an unrelated document about pottery", "title": "pots", "views": 1},
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
    client.indices.create(index=INDEX)
    check(client.indices.exists(index=INDEX), "the index exists after create")

    print("documents")
    for doc_id, body in DOCS.items():
        client.index(index=INDEX, id=doc_id, body=body, refresh=True)
    got = client.get(index=INDEX, id="rrf")
    check(got["found"] is True, "get reports found")
    check(got["_source"] == DOCS["rrf"],
          f"_source round-trips unchanged (got {json.dumps(got['_source'], sort_keys=True)})")

    print("search")
    res = client.search(index=INDEX, body={"query": {"match": {"text": "rank fusion"}}, "size": 10})
    ids = [h["_id"] for h in res["hits"]["hits"]]
    check(ids, f"match returned hits (got {ids})")
    check(ids[0] == "rrf", f"the document both token streams rank comes first (got {ids})")
    check("pottery" not in ids, f"a document holding neither term is absent (got {ids})")
    check(res["hits"]["hits"][0]["_source"] == DOCS["rrf"], "a hit carries the body that was indexed")

    everything = client.search(index=INDEX, body={"query": {"match_all": {}}})
    check(everything["hits"]["total"]["value"] == len(DOCS),
          f"match_all counts every document (got {everything['hits']['total']['value']})")

    print("refusals — D-026")
    for name, body, want in [
        ("aggs", {"aggs": {"v": {"terms": {"field": "title"}}}}, 501),
        ("highlight", {"query": {"match": {"text": "fusion"}}, "highlight": {"fields": {"text": {}}}}, 501),
        ("sort", {"query": {"match": {"text": "fusion"}}, "sort": [{"views": "asc"}]}, 400),
        ("bool", {"query": {"bool": {"must": [{"match": {"text": "fusion"}}]}}}, 400),
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

    print("teardown")
    client.delete(index=INDEX, id="pottery", refresh=True)
    check(not client.exists(index=INDEX, id="pottery"), "a deleted document is gone")
    client.indices.delete(index=INDEX)
    check(not client.indices.exists(index=INDEX), "the index is gone after delete")

    print("\nPASS: opensearch-py drove weftd unmodified.")


if __name__ == "__main__":
    sys.exit(main())
