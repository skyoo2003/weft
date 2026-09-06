.PHONY: all fmt build vet test lint lint-if-present lint-docs lint-docs-if-present spdx fuzz arch deps run serve serve-grpc compat compat-grpc grpc-build example clean \
	changelog changelog-new changelog-check docs-site release-check \
	eval eval-full eval-data recall bench bench-preflight bench-compare bench-build bench-head bench-http

# `all` needs nothing installed beyond the Go toolchain, which is what lets a
# first-time contributor run the whole gate before they have read anything.
# `fuzz` costs something `all` should not — a minute of wall clock — so it is
# named separately and CI runs it as its own step.
#
# The two linters reach `all` through -if-present targets rather than directly,
# which keeps the no-tool-required property above while closing the gap it
# opened: for five commits `make all` passed a tree CI rejected, because the only
# gates reading .golangci.yaml and .markdownlint.yaml lived in CI. Both skip when
# their tool is absent, so a first checkout still runs the gate; both run for
# everyone who has the tool, so CI stops being where a finding is first seen.
all: fmt build vet test lint-if-present lint-docs-if-present

# go vet says nothing about formatting, so drift would otherwise surface in
# review instead of before the commit. gofmt's exit status carries as much as
# its output: a file it cannot parse prints nothing to stdout, which would read
# as clean. Build, vet and test would not catch it either if the file is behind
# a build tag for another platform.
fmt:
	@files=$$(gofmt -l .) || { echo "FAIL: gofmt errored"; exit 1; }; \
	if [ -n "$$files" ]; then \
		echo "FAIL: not gofmt'd:"; echo "$$files"; exit 1; \
	else \
		echo "OK: gofmt clean"; \
	fi

build:
	go build ./...

vet:
	go vet ./...

# -race by default: scorer/text has a concurrent-writer test that only means
# anything under the detector.
test:
	go test -race ./...

# Pinned in one place. CI passes this same string to golangci-lint-action, so
# a local run and a CI run judge with the same rules; the version lives here
# rather than in .golangci.yaml because that file has no field for it.
GOLANGCI_VERSION := v2.12.2

lint:
	@command -v golangci-lint >/dev/null || { \
		echo "FAIL: golangci-lint is not installed."; \
		echo "  brew install golangci-lint   # or see https://golangci-lint.run/welcome/install/"; \
		echo "  CI pins $(GOLANGCI_VERSION); a different local version can disagree."; \
		exit 1; \
	}
	# Warned, not failed. A newer local binary is the normal state after a brew
	# upgrade and mostly agrees; what it cannot do is promise that a clean local
	# run means a clean CI run, and that promise is the only reason this target
	# is in `all`. So the mismatch is said out loud rather than assumed away.
	@v=$$(golangci-lint version --short 2>/dev/null); \
	[ "v$$v" = "$(GOLANGCI_VERSION)" ] || \
		echo "WARN: golangci-lint v$$v locally, CI pins $(GOLANGCI_VERSION) — the two can disagree"
	golangci-lint run ./...
	# bench/ too, for the reason bench-build exists: `./...` does not descend into a
	# nested module, so without this line bench/main.go is the one committed .go file
	# in the repository no lint gate ever reads. It picks up this same .golangci.yaml
	# by walking up from bench/.
	cd bench && golangci-lint run ./...
	# grpc/ too, and for the identical reason: a second nested module is a second
	# place `./...` does not reach.
	cd grpc && golangci-lint run ./...

# `lint`, minus the refusal to run without the tool. This is what `all` calls, so
# that the gate a contributor runs and the gate CI runs judge the same rules
# whenever the tool is there to judge with. `make lint` stays the one that fails
# on a missing binary: asked for by name, a silent skip is the wrong answer.
#
# One shell, not two, for the reason `deps` and `recall` below are written the same
# way: each recipe line gets its own, so an `exit 0` in the first skips nothing that
# follows it. Written as two lines this printed SKIP and then ran the lint it had
# just said it was skipping, failing a checkout with no golangci-lint on it — which
# is the property this target exists to preserve. `if` reports the status of the
# branch it took, so a real lint failure still fails the gate.
lint-if-present:
	@if command -v golangci-lint >/dev/null; then \
		$(MAKE) --no-print-directory lint; \
	else \
		echo "SKIP: golangci-lint is not installed — CI will lint this. 'make lint' says how to install it."; \
	fi

# Markdown is most of what a first-time reader of this repository actually
# reads, so it gets the same treatment as the Go.
#
# npx is accepted as well as a global install, because the alternative was what
# happened on this branch: neither was on the machine, `make lint-docs` refused,
# and 76 findings in five documents went unread until CI's Go lint stopped
# failing first and let the docs step run. Anyone with node can now run the docs
# gate without installing anything permanently.
lint-docs:
	@if command -v markdownlint-cli2 >/dev/null; then \
		markdownlint-cli2 "**/*.md"; \
	elif command -v npx >/dev/null; then \
		echo "using npx; 'npm install -g markdownlint-cli2' is faster if you do this often"; \
		npx --yes markdownlint-cli2 "**/*.md"; \
	else \
		echo "FAIL: markdownlint-cli2 is not installed and there is no npx."; \
		echo "  npm install -g markdownlint-cli2"; \
		exit 1; \
	fi

# The docs half of lint-if-present, and there for the same reason: the gate that
# only CI runs is the gate nobody runs. Skips when there is nothing to run it
# with, so `all` still works on a checkout with no node on it.
lint-docs-if-present:
	@if command -v markdownlint-cli2 >/dev/null || command -v npx >/dev/null; then \
		$(MAKE) --no-print-directory lint-docs; \
	else \
		echo "SKIP: no markdownlint-cli2 and no npx — CI will lint the docs."; \
	fi

# `go test` replays the seed corpus and stops; it never starts the fuzzing
# engine. SECURITY.md names the segment decoder as the first place a hostile
# file lands, and these two targets are what asks it questions nobody wrote
# down. Out of `all` because a minute is too long to pay on every local run.
FUZZTIME ?= 30s

fuzz:
	go test -fuzz=FuzzSegmentDecoding -fuzztime=$(FUZZTIME) -run '^$$' ./pkg/engine
	go test -fuzz=FuzzParseSection -fuzztime=$(FUZZTIME) -run '^$$' ./pkg/engine
	go test -fuzz=FuzzParseDSL -fuzztime=$(FUZZTIME) -run '^$$' ./internal/opensearch
	go test -fuzz=FuzzParseNative -fuzztime=$(FUZZTIME) -run '^$$' ./internal/opensearch
	go test -fuzz=FuzzParseBulk -fuzztime=$(FUZZTIME) -run '^$$' ./internal/opensearch

# Every .go file carries its licence in a line a machine can find, which is
# what an SPDX scanner reads when this repository is vendored into another.
spdx:
	@missing=$$(git ls-files '*.go' | xargs grep -L -E '^// SPDX-License-Identifier:' || true); \
	if [ -n "$$missing" ]; then \
		echo "FAIL: missing SPDX headers in:"; echo "$$missing"; \
		echo "run: make spdx-fix"; exit 1; \
	else \
		echo "OK: every .go file has an SPDX header"; \
	fi

.PHONY: spdx-fix
spdx-fix:
	@for f in $$(git ls-files '*.go' | xargs grep -L -E '^// SPDX-License-Identifier:' || true); do \
		{ printf '// SPDX-License-Identifier: Apache-2.0\n\n'; cat $$f; } > $$f.tmp && mv $$f.tmp $$f; \
		echo "added: $$f"; \
	done

# Changelog entries are written as the change is made, into changes/unreleased/,
# and CHANGELOG.md is generated from them. The "## Unreleased" heading is passed
# on every merge so that what is pending is visible in the file rather than only
# in a directory nobody browses.
# The backslashes are Make's, not changie's: an unescaped `#` starts a comment,
# so this would assign the empty string and both targets below would quietly
# drop every pending fragment — including the check that exists to catch that.
UNRELEASED_HEADING := \#\# Unreleased

changelog-new:
	@command -v changie >/dev/null || { echo "FAIL: changie is not installed. brew install changie"; exit 1; }
	changie new

changelog:
	@command -v changie >/dev/null || { echo "FAIL: changie is not installed. brew install changie"; exit 1; }
	changie merge -u '$(UNRELEASED_HEADING)'

# CHANGELOG.md is generated, so the failure mode is someone editing it by hand
# and losing the edit at the next merge. This rewrites and diffs, which means a
# failure leaves the corrected file in the working tree ready to stage — the
# same shape as gofmt.
# Compares against a rendering rather than against git: asking `git diff` would
# also fail on a CHANGELOG.md that is correct but simply not committed yet,
# which is the normal state in the middle of the change that generated it.
changelog-check:
	@command -v changie >/dev/null || { echo "SKIP: changie not installed"; exit 0; }
	@changie merge -u '$(UNRELEASED_HEADING)' --dry-run > .changelog.expected
	@if diff -q CHANGELOG.md .changelog.expected >/dev/null 2>&1; then \
		rm -f .changelog.expected; echo "OK: CHANGELOG.md matches changes/"; \
	else \
		echo "FAIL: CHANGELOG.md is not what changie generates from changes/. Run 'make changelog'."; \
		diff -u CHANGELOG.md .changelog.expected | head -40; \
		rm -f .changelog.expected; exit 1; \
	fi

# The docs site mounts /docs rather than copying it, so this renders the very
# files GitHub renders. `hugo server -s site` is the one to use while writing.
docs-site:
	@command -v hugo >/dev/null || { echo "FAIL: hugo is not installed. brew install hugo"; exit 1; }
	hugo --source site --destination public --gc --minify

# Catches a broken release pipeline at edit time rather than at tag time, when
# the tag is already unwithdrawable.
release-check:
	@command -v goreleaser >/dev/null || { echo "FAIL: goreleaser is not installed. brew install goreleaser"; exit 1; }
	goreleaser check
	goreleaser build --snapshot --clean --single-target

# The milestone 1 pass/fail line: fusion is invariant to scorer count, a fourth
# scorer costs under 100 lines, and fusion cannot see any scorer.
arch:
	go test -v -run 'TestAddingAFourthScorer|TestAnyNumberOfScorers|TestFourthScorerIsUnderOneHundredLines|TestEngineAPISurface|TestPublicAPISurface|TestNeitherEngineNorFusion|TestGoListDeps|TestNoExternalDependencies' ./pkg/engine/

# The two architecture properties cheap enough to check by hand.
deps:
	@echo "--- external dependencies (want: this module and nothing else) ---"
	@GOWORK=off go list -m all
	@echo "--- what fusion can see (want: nothing named scorer) ---"
	@if GOWORK=off go list -deps ./pkg/fusion | grep '/scorer/'; then \
		echo "FAIL: fusion imports a scorer package"; exit 1; \
	else \
		echo "OK: fusion imports no scorer package"; \
	fi

# Where the eval subcommands keep their downloads and generated artifacts. Matches
# the -data default in cmd/weft-eval, and is here so eval-data can check for a file
# it does not generate itself.
EVAL_DATA ?= .eval-data

# Milestone 4. The headline numbers, reprinted from an already-built index.
# docs/EVAL.md is the measurement design and the judgment rule; this target exists
# so a published figure can be re-derived with one command.
eval:
	go run ./cmd/weft-eval run

# Milestone 3b. What the approximate vector index costs and what it buys, which
# `eval` cannot say: nDCG is blind to a partition dropping neighbours the qrels
# never judged, so recall against a brute-force scan is measured separately.
#
# Skipped rather than failed without the data, unlike `eval`. This target exists
# to be run by anyone reproducing the FINDINGS numbers, and a missing multi-
# gigabyte download is not a broken checkout.
#
# One shell, not two: each recipe line gets its own, so an `exit 0` in the first
# skips nothing that follows it. Same shape as `deps` above.
recall:
	@if [ ! -d $(EVAL_DATA)/index ]; then \
		echo "SKIP: no index at $(EVAL_DATA)/index — run 'make eval-data' first"; \
	else \
		go run ./cmd/weft-eval recall -data $(EVAL_DATA); \
	fi

# Milestone 5. The latency distribution `eval` and `recall` cannot produce: both
# report means of a sequential replay, and a tail is the statistic a mean is blind
# to. Open loop, so a stalled server does not get to slow the load down and hide
# its own p99 — docs/PERF.md section 2.
#
# Not in `all` and not in CI. A shared runner's tail latency is a function of
# whatever else is on the machine, so gating a merge on a p99 measured there makes
# the gate a coin flip. CI runs the driver's unit tests as part of `all` and compiles
# the bleve side through `bench-build`; the numbers themselves are produced by a
# person on a quiet machine and published in docs/PERF.md.
#
# Long: the ladder's lowest rung sends 10,000 queries at an eighth of measured
# throughput, and a p99 needs all 10,000 — see internal/loadgen.Printable.
bench:
	@if [ ! -d $(EVAL_DATA)/index ]; then \
		echo "SKIP: no index at $(EVAL_DATA)/index — run 'make eval-data' first"; \
	else \
		go run ./cmd/weft-eval bench -data $(EVAL_DATA) $(BENCHFLAGS); \
	fi

# Milestone 14. Run this before `bench`, and read two lines off it.
#
# Twice the ladder above has been spent on a machine that could not reproduce its own
# published figures, and both times the finding came after the fact: milestone 11's
# run A died in the index load, milestone 13's came back void after 97 minutes. What
# made the second one void is on the record in docs/PERF.md section 5.6 — and the
# reading that would have caught it is not the unloaded median. That one was *normal*
# on the void machine, 34.072 ms against a published 32.231. The only cheap signal
# that separates them is the top rate under load: 1.04x published, 78x void.
#
# So: the top rung alone, 10 rotations instead of 200. About a minute.
#
# PASS: the rung's p50 is at most twice the `unloaded: p50` line this same run
# printed, and shed is 0. Twice is loadgen.SaturationRate's constant rather than a
# number picked here — the first rung past twice the unloaded median is saturation,
# and 27.28/s was not saturation on the published ladder. A machine that saturates
# here cannot reproduce it, and the honest move is to not spend the 97 minutes.
#
# These numbers are not published. They decide whether the ladder runs.
#
# A separate process on purpose: peakrss is ru_maxrss, which the kernel never lowers,
# and the memory clause reads the *ladder's* peak (docs/PERF.md section 2.7, D-014).
# Folded into the ladder, this probe would raise the mark before rung 1 reported.
#
# ponytail: a documented step, not an enforced one — the comparison is a person
# reading two printed lines. The two failures were not a failed comparison, they were
# a probe nobody ran. If a fourth ladder still comes back void, this becomes a
# -preflight flag with an exit code (~30 lines in cmd/weft-eval/bench.go).
bench-preflight:
	@if [ ! -d $(EVAL_DATA)/index ]; then \
		echo "SKIP: no index at $(EVAL_DATA)/index — run 'make eval-data' first"; \
	else \
		go run ./cmd/weft-eval bench -data $(EVAL_DATA) -rates 27.28 -rotations 10; \
	fi

# Milestone 27: the same ladder, through a socket.
#
# Two processes on one machine, which is deliberate and is the comparison's whole
# point — the in-process ladder measured the same corpus on the same hardware, so
# the difference between the two runs is the HTTP surface and the second process,
# and nothing else. The address is loopback because weftd binds loopback.
#
# It starts weftd itself and stops it afterwards, because a run against a server
# somebody started by hand is a run whose data directory and uptime are not
# recorded anywhere. Everything the report says about memory and the collector
# comes from that server's GET /_nodes/stats and not from the load generator —
# cmd/weft-eval/benchhttp.go is the argument for why there is no fallback.
#
# It needs the same quiet window every ladder needs. `make bench-preflight` first,
# and the lid open: docs/PERF.md section 5.7 and D-024.
WEFTD_ADDR ?= 127.0.0.1:9200
WEFTD_INDEX ?= papers

bench-http:
	@if [ ! -d $(EVAL_DATA)/index ]; then \
		echo "SKIP: no index at $(EVAL_DATA)/index — run 'make eval-data' first"; exit 0; \
	fi; \
	go run ./cmd/weftd -addr $(WEFTD_ADDR) -data $(EVAL_DATA)/weftd & \
	pid=$$!; \
	trap "kill $$pid 2>/dev/null" EXIT INT TERM; \
	until curl -sf http://$(WEFTD_ADDR)/ >/dev/null 2>&1; do \
		kill -0 $$pid 2>/dev/null || { echo "FAIL: weftd exited before it listened"; exit 1; }; \
		sleep 1; \
	done; \
	go run ./cmd/weft-eval bench -data $(EVAL_DATA) -http $(WEFTD_ADDR) -http-index $(WEFTD_INDEX) $(BENCHFLAGS)

# Milestone 5's third assertion: same machine, same corpus, same queries, same
# driver. bench/ is a separate module so that bleve never enters this one — `make
# deps` is what proves it did not. See bench/README.md for what the comparison
# does and does not cover.
bench-compare:
	@if [ ! -d bench/.bleve-index ]; then \
		echo "SKIP: no bleve index — run 'cd bench && go run . -build' first (slow, once)"; \
	else \
		cd bench && go run . -data $(abspath $(EVAL_DATA)) $(BENCHFLAGS); \
	fi

# The bleve side type-checked and tested. Its own target because it is its own
# module: `go build ./...` at the root does not descend into a nested module, so
# without this nothing in the gate ever compiles bench/main.go and the comparison
# rots silently between the runs that use it.
#
# `go build ./...` is not among the three: it type-checks nothing vet does not, and it
# writes a 20 MiB executable into bench/ that nothing runs — the binary .gitignore had
# to be taught about and `make clean` did not remove. vet fails on a compile error,
# which is the guarantee this target exists for.
#
# `go test ./...` is here for when bench/ has tests rather than because it has them
# now: the parsing it used to own moved to internal/eval, which is tested in the root
# module, and what is left is the bleve calls themselves.
bench-build:
	cd bench && go vet ./... && go test ./...

# The head-to-head against bleve, on a corpus this generates rather than one that
# has to be downloaded.
#
# It is not the ladder and does not replace it: `bench` measures a tail under
# open-loop load against the prepared TREC-COVID index, and this measures what
# one query costs sequentially. What it buys is that the second question can be
# answered on a laptop in under a minute, against an engine that is not weft — so
# a change to the query path has something to be checked against without waiting
# on a 90-minute run that has come back void three rounds running.
#
# Read both columns. Time and allocation move independently here, and the
# allocation column is the one docs/FINDINGS.md milestone 22 is about.
bench-head:
	cd bench && go test -run '^$$' -bench HeadToHead -benchtime 50x

# Everything milestone 4 publishes: the degeneracy diagnostic, the frozen arms, the
# sensitivity sweep, and the fusion weight sweep behind the README's claim that no
# weight makes the graph stream worth anything. Slower — the sweep alone re-measures
# 28 configurations.
eval-full:
	go run ./cmd/weft-eval diagnose
	go run ./cmd/weft-eval run
	go run ./cmd/weft-eval sweep
	go run ./cmd/weft-eval weights

# One-time data preparation. prepare is rate limited and takes hours; it appends
# and is resumable, so rerunning it continues rather than starting over. See
# docs/EVAL.md section 3 for the downloads it expects to already be in place.
#
# The query vectors sit between the two Go steps and are not one of them: they come
# out of a PyTorch model this repository deliberately does not depend on, so the step
# is checked rather than run. Without it `make eval` still succeeds — loadQueries
# only warns, the vector scorer abstains, and arms printed as `text+vector` are
# text-only — which is the binding experiment quietly measuring something else. So
# this target refuses to call preparation complete until the file exists.
eval-data:
	go run ./cmd/weft-eval prepare
	@if [ ! -f $(EVAL_DATA)/query-vectors.jsonl ]; then \
		echo "FAIL: $(EVAL_DATA)/query-vectors.jsonl does not exist."; \
		echo "  Without it the vector scorer abstains and the arms printed as"; \
		echo "  'text+vector' are text-only — see docs/EVAL.md section 5.4."; \
		echo "  Generate it with the venv from docs/EVAL.md section 7:"; \
		echo "    python internal/eval/testdata/gen_query_vectors.py --verify"; \
		echo "    python internal/eval/testdata/gen_query_vectors.py"; \
		echo "  Then rerun 'make eval-data'."; \
		exit 1; \
	fi
	go run ./cmd/weft-eval build

# The default data directory both servers read, so `make serve` and `make serve-grpc`
# in two terminals are two surfaces over one index rather than two empty ones.
WEFTD_DATA ?= .weftd-data

run:
	go run ./cmd/weft

serve:
	go run ./cmd/weftd

# Milestone 24's judgment sentence: an official client drives weftd unmodified.
#
# Not in `all`, and skipped rather than failed when the client is absent — the
# property that `make all` needs nothing beyond the Go toolchain is the one this
# repository has protected since the first commit, and a compatibility check
# that broke it would be paid for on every checkout. CI runs this as its own
# step, the way `fuzz` is run.
#
# One shell, not two. Each recipe line gets its own, so an `exit 0` in the first
# would skip nothing that follows it — the trap `lint-if-present` and `recall`
# are both written around, and the one that once made a target print SKIP and
# then run the very thing it had just said it was skipping.
#
# The Go tests already drive every one of these routes through httptest. What
# only a real client can say is whether the fields it reads are the fields weftd
# sends, and that is what D-026's version claim is spent on.
# PYTHON is overridable so the client can live in a throwaway virtualenv rather
# than in the interpreter on PATH — the same shape docs/EVAL.md section 7 uses
# for its reference implementations, and for the same reason: a check that
# demands a global install is a check people stop running.
#
#	python3 -m venv /tmp/weft-compat-venv
#	/tmp/weft-compat-venv/bin/pip install opensearch-py
#	make compat PYTHON=/tmp/weft-compat-venv/bin/python
PYTHON ?= python3

compat:
	@if command -v $(PYTHON) >/dev/null && $(PYTHON) -c 'import opensearchpy' >/dev/null 2>&1; then \
		$(PYTHON) internal/opensearch/testdata/compat.py; \
	else \
		echo "SKIP: $(PYTHON) has no opensearch-py. See the PYTHON note above this target in the Makefile."; \
	fi

# Milestone 31's judgment sentence: a stock gRPC client drives weftg unmodified.
#
# Same shape as `compat` and there for the same reason. The Go tests in grpc/
# already drive every one of these calls in-process; what only a foreign
# toolchain can say is whether the descriptor this repository ships is one it can
# compile and talk to. The script generates its own stubs from weft.proto, so a
# .proto that drifted from the service fails here and nowhere else.
#
#	python3 -m venv /tmp/weft-grpc-venv
#	/tmp/weft-grpc-venv/bin/pip install grpcio grpcio-tools
#	make compat-grpc PYTHON=/tmp/weft-grpc-venv/bin/python
compat-grpc:
	@if command -v $(PYTHON) >/dev/null && $(PYTHON) -c 'import grpc_tools' >/dev/null 2>&1; then \
		$(PYTHON) grpc/testdata/compat_grpc.py; \
	else \
		echo "SKIP: $(PYTHON) has no grpcio-tools. See the note above this target in the Makefile."; \
	fi

# The gRPC side type-checked and tested. Its own target because it is its own
# module, exactly as bench-build is: `go build ./...` at the root does not descend
# into a nested module, so without this nothing in the gate ever compiles grpc/
# and it rots silently between the runs that use it.
#
# It also re-checks the generated code against the .proto by compiling both, which
# is the cheap half of what compat-grpc pays for properly.
grpc-build:
	cd grpc && go vet ./... && go test ./...

serve-grpc:
	cd grpc && go run ./cmd/weftg -data ../$(WEFTD_DATA)

example:
	go run ./examples/basic

clean:
	go clean ./...
	rm -f weft
	# bench/ is its own module, so `go clean ./...` above does not reach it, and a
	# `go build` run there by hand leaves a 20 MiB executable named after the directory.
	cd bench && go clean ./...
	rm -f bench/bench
	cd grpc && go clean ./...
