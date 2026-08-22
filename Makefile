.PHONY: all fmt build vet test lint lint-if-present lint-docs spdx fuzz arch deps run example clean \
	changelog changelog-new changelog-check docs-site release-check \
	eval eval-full eval-data recall bench bench-compare bench-build

# `all` needs nothing installed beyond the Go toolchain, which is what lets a
# first-time contributor run the whole gate before they have read anything.
# `lint-docs` and `fuzz` each cost something `all` should not — a tool to
# install, or a minute of wall clock — so they are named separately and CI runs
# them as their own steps.
#
# lint reaches `all` through lint-if-present rather than directly, which keeps
# the no-tool-required property above while closing the gap it opened: for five
# commits `make all` passed a tree CI's lint step rejected, because the only
# gate reading .golangci.yaml lived in CI. Skipped when the tool is absent, so a
# first checkout still runs the gate; run for everyone who has it, so CI stops
# being where a lint finding is first seen.
all: fmt build vet test lint-if-present

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
lint-docs:
	@command -v markdownlint-cli2 >/dev/null || { \
		echo "FAIL: markdownlint-cli2 is not installed."; \
		echo "  npm install -g markdownlint-cli2"; \
		exit 1; \
	}
	markdownlint-cli2 "**/*.md"

# `go test` replays the seed corpus and stops; it never starts the fuzzing
# engine. SECURITY.md names the segment decoder as the first place a hostile
# file lands, and these two targets are what asks it questions nobody wrote
# down. Out of `all` because a minute is too long to pay on every local run.
FUZZTIME ?= 30s

fuzz:
	go test -fuzz=FuzzSegmentDecoding -fuzztime=$(FUZZTIME) -run '^$$' ./pkg/engine
	go test -fuzz=FuzzParseSection -fuzztime=$(FUZZTIME) -run '^$$' ./pkg/engine

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

run:
	go run ./cmd/weft

example:
	go run ./examples/basic

clean:
	go clean ./...
	rm -f weft
	# bench/ is its own module, so `go clean ./...` above does not reach it, and a
	# `go build` run there by hand leaves a 20 MiB executable named after the directory.
	cd bench && go clean ./...
	rm -f bench/bench
