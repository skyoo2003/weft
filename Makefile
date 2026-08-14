.PHONY: all fmt build vet test arch deps run example clean

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

run:
	go run ./cmd/weft

example:
	go run ./examples/basic

clean:
	go clean ./...
	rm -f weft
