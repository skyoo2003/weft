.PHONY: all fmt build vet test lint lint-docs spdx fuzz arch deps run example clean \
	changelog changelog-new changelog-check docs-site release-check \
	eval eval-full eval-data

# `all` needs nothing installed beyond the Go toolchain, which is what lets a
# first-time contributor run the whole gate before they have read anything.
# `lint`, `lint-docs` and `fuzz` each cost something `all` should not — a tool
# to install, or a minute of wall clock — so they are named separately and CI
# runs them as their own steps.
all: fmt build vet test

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
	golangci-lint run ./...

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

# Milestone 4. The headline numbers, reprinted from an already-built index.
# docs/EVAL.md is the measurement design and the judgment rule; this target exists
# so a published figure can be re-derived with one command.
eval:
	go run ./cmd/weft-eval run

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
eval-data:
	go run ./cmd/weft-eval prepare
	go run ./cmd/weft-eval build

run:
	go run ./cmd/weft

example:
	go run ./examples/basic

clean:
	go clean ./...
	rm -f weft
