.PHONY: all build vet test arch deps run example clean

all: build vet test

build:
	go build ./...

vet:
	go vet ./...

test:
	go test ./...

# The milestone 1 pass/fail line: fusion is invariant to scorer count, a fourth
# scorer costs under 100 lines, and fusion cannot see any scorer.
arch:
	go test -v -run 'TestAddingAFourthScorer|TestAnyNumberOfScorers|TestFourthScorerIsUnderOneHundredLines|TestNeitherEngineNorFusion|TestGoListDeps|TestNoExternalDependencies' ./pkg/engine/

# The two architecture properties cheap enough to check by hand.
deps:
	@echo "--- external dependencies (want: this module and nothing else) ---"
	@go list -m all
	@echo "--- what fusion can see (want: nothing named scorer) ---"
	@if go list -deps ./pkg/fusion | grep '/scorer/'; then \
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
