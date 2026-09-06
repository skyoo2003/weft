# SPDX-License-Identifier: Apache-2.0
"""Drive weftg with a stock gRPC client, generated from weft.proto and nothing else.

This is `make compat`'s argument for the second wire. The Go tests already drive
every one of these calls in-process; what only a real client can say is whether
the descriptor weft ships is one a foreign toolchain can compile and talk to. If
grpcio-tools cannot read weft.proto, or the fields it generates are not the fields
weftg sends, the Go tests would never notice.

Two processes, because the two surfaces have different jobs: weftd writes and
weftg searches. Documents go in over HTTP and come out over gRPC, on one data
directory — which is also the claim being checked.

    python3 -m venv /tmp/weft-grpc-venv
    /tmp/weft-grpc-venv/bin/pip install grpcio grpcio-tools
    make compat-grpc PYTHON=/tmp/weft-grpc-venv/bin/python
"""

import json
import os
import pathlib
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

HERE = pathlib.Path(__file__).resolve().parent
GRPC_MODULE = HERE.parent  # the nested Go module
ROOT = GRPC_MODULE.parent

HTTP_ADDR = "127.0.0.1:9298"
GRPC_ADDR = "127.0.0.1:9299"

failures = 0


def check(ok, label, detail=""):
    global failures
    if ok:
        print(f"  ok  {label}")
    else:
        failures += 1
        print(f"  FAIL {label}{': ' + detail if detail else ''}")


def http(method, path, body=None, want=200):
    req = urllib.request.Request(
        f"http://{HTTP_ADDR}{path}",
        method=method,
        data=json.dumps(body).encode() if body is not None else None,
        headers={"Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req) as resp:
            status, raw = resp.status, resp.read()
    except urllib.error.HTTPError as e:
        status, raw = e.code, e.read()
    if status != want:
        raise SystemExit(f"{method} {path}: status {status}, want {want} ({raw!r})")
    return json.loads(raw) if raw else {}


def require_free(addr, what):
    """Refuse to start if something is already listening.

    Not defensive clutter — this is the failure this script actually had. A
    leftover server from an earlier run held the port, the new one exited with
    "address already in use", and every check below then ran green or red against
    *somebody else's process and somebody else's data directory*. A stale index
    made it look like weftd had rejected a mapping it had never been sent.

    A check that can silently grade the wrong program is worse than no check.
    """
    host, port = addr.rsplit(":", 1)
    with socket.socket() as s:
        s.settimeout(1)
        if s.connect_ex((host, int(port))) == 0:
            raise SystemExit(
                f"something is already listening on {addr}, so {what} could not be the thing "
                f"answering these checks. Stop it first: lsof -ti tcp:{port} | xargs kill"
            )


def spawn(argv, cwd):
    """Start a server in its own process group.

    start_new_session so that terminate() below reaches the binary `go run`
    compiled and not only `go run` itself — without it the search server outlives
    this script and holds the port against the next run, which is exactly how the
    stale-server failure above happened.
    """
    return subprocess.Popen(argv, cwd=cwd, start_new_session=True)


def wait(proc, probe, what):
    for _ in range(600):
        if proc.poll() is not None:
            raise SystemExit(f"{what} exited before it listened (rc={proc.returncode})")
        try:
            probe()
            return
        except Exception:  # noqa: BLE001 - any failure to reach it means it is not up yet
            time.sleep(0.1)
    raise SystemExit(f"{what} never listened on its address")


def main():
    try:
        import grpc  # noqa: F401
        from grpc_tools import protoc
    except ImportError:
        print("SKIP: this interpreter has no grpcio/grpcio-tools. See the docstring above.")
        return 0

    out = pathlib.Path(tempfile.mkdtemp(prefix="weft-grpc-stubs-"))
    data = pathlib.Path(tempfile.mkdtemp(prefix="weft-grpc-data-"))
    procs = []
    try:
        # 1. The client is generated here, from the .proto in the repository. That
        #    is the whole point: nothing pre-generated and nothing hand-adjusted.
        rc = protoc.main([
            "protoc",
            f"-I{GRPC_MODULE}",
            f"--python_out={out}",
            f"--grpc_python_out={out}",
            str(GRPC_MODULE / "weft.proto"),
        ])
        check(rc == 0, "grpcio-tools compiles weft.proto", f"protoc returned {rc}")
        if rc != 0:
            return 1
        sys.path.insert(0, str(out))
        import grpc
        import weft_pb2
        import weft_pb2_grpc

        # 2. weftd writes, weftg reads. One data directory.
        require_free(HTTP_ADDR, "weftd")
        require_free(GRPC_ADDR, "weftg")
        procs.append(spawn(["go", "run", "./cmd/weftd", "-addr", HTTP_ADDR, "-data", str(data)], ROOT))
        wait(procs[-1], lambda: http("GET", "/"), "weftd")

        http("PUT", "/papers",
             {"mappings": {"properties": {"vec": {"type": "knn_vector", "dimension": 3}}}})
        for doc_id, body in [
            ("rrf", {"text": "reciprocal rank fusion combines rankings", "vec": [1, 0, 0]}),
            ("bm25", {"text": "bm25 ranks documents by term frequency", "vec": [0.9, 0.1, 0]}),
            ("pottery", {"text": "an unrelated document about pottery", "vec": [0, 1, 0]}),
        ]:
            http("PUT", f"/papers/_doc/{doc_id}?refresh=true", body, want=201)

        procs.append(spawn(
            ["go", "run", "./cmd/weftg", "-addr", GRPC_ADDR, "-data", str(data)], GRPC_MODULE))

        channel = grpc.insecure_channel(GRPC_ADDR)
        stub = weft_pb2_grpc.WeftStub(channel)
        wait(procs[-1],
             lambda: stub.Search(weft_pb2.SearchRequest(
                 index="papers", streams=['{"match":{"text":"pottery"}}']), timeout=1),
             "weftg")

        # 3. A fusion of two signals, named positionally, weighted positionally.
        res = stub.Search(weft_pb2.SearchRequest(
            index="papers",
            streams=['{"match":{"text":"rank fusion"}}',
                     '{"knn":{"vec":{"vector":[1, 0, 0],"k":2}}}'],
            weights=[1, 0.5], size=5, breakdown=True))
        ids = [h.id for h in res.hits]
        check(len(ids) > 0, "a two-signal fusion returns hits", f"got {ids}")
        check("rrf" in ids, "the text stream reached the ranking", f"got {ids}")

        # 4. The same ranking the HTTP surface produces, because it is one engine.
        over_http = http("POST", "/papers/_weft/search", {
            "streams": [json.loads('{"match":{"text":"rank fusion"}}'),
                        json.loads('{"knn":{"vec":{"vector":[1, 0, 0],"k":2}}}')],
            "weights": [1, 0.5], "size": 5})
        want = [h["_id"] for h in over_http["hits"]["hits"]]
        check(ids == want, "gRPC and HTTP rank identically", f"{ids} vs {want}")

        # 5. The breakdown crosses the wire with absence intact.
        by_id = {h.id: h.breakdown for h in res.hits}
        check(all(len(b) == 2 for b in by_id.values()),
              "one breakdown column per stream the request named",
              str({k: len(v) for k, v in by_id.items()}))
        check(any(not r.present for b in by_id.values() for r in b),
              "a stream with no opinion arrives as present=false and not as rank 0")

        # 6. A source is JSON the client can read.
        check(all(json.loads(h.source) for h in res.hits), "every hit carries decodable _source")

        # 7. Refusals are codes, never empty results. D-026 on the second wire.
        for label, req, want_code, says in [
            ("an unknown index", weft_pb2.SearchRequest(
                index="nope", streams=['{"match":{"text":"a"}}']),
             grpc.StatusCode.NOT_FOUND, "nope"),
            ("no streams", weft_pb2.SearchRequest(index="papers"),
             grpc.StatusCode.INVALID_ARGUMENT, "stream"),
            ("an unregistered clause", weft_pb2.SearchRequest(
                index="papers", streams=['{"mach":{"text":"a"}}']),
             grpc.StatusCode.INVALID_ARGUMENT, "mach"),
            ("a query this engine cannot express", weft_pb2.SearchRequest(
                index="papers", streams=['{"query_string":{"query":"a"}}']),
             grpc.StatusCode.UNIMPLEMENTED, "query string"),
        ]:
            try:
                stub.Search(req)
                check(False, f"{label} is refused", "it answered instead")
            except grpc.RpcError as e:
                check(e.code() == want_code, f"{label} is {want_code.name}", f"got {e.code().name}")
                check(says in e.details(), f"{label} carries a reason", e.details())

        print()
        if failures:
            print(f"FAIL: {failures} check(s) failed.")
            return 1
        print("PASS: a stock gRPC client, generated from weft.proto, drove weftg unmodified.")
        return 0
    finally:
        for p in procs:
            # The group, not the process: `go run` forks the binary it built, and
            # signalling only the parent leaves the server holding the port.
            try:
                os.killpg(os.getpgid(p.pid), signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                p.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(os.getpgid(p.pid), signal.SIGKILL)
        shutil.rmtree(out, ignore_errors=True)
        shutil.rmtree(data, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
