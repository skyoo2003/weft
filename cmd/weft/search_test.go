// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// indexed builds the three-document corpus on disk and returns its directory.
func indexed(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ix")
	if code, _, stderr := feed(t, threeDocs, "index", "-data", dir); code != 0 {
		t.Fatalf("index: exit %d, stderr:\n%s", code, stderr)
	}
	return dir
}

// first returns the key on the first result line. Every output form of this
// command starts a result with a rank and then the key.
func first(t *testing.T, stdout string) string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasSuffix(fields[0], ".") {
			return fields[1]
		}
	}
	t.Fatalf("no result line in:\n%s", stdout)
	return ""
}

func TestSearchByText(t *testing.T) {
	dir := indexed(t)

	code, stdout, stderr := feed(t, "", "search", "-data", dir, "-scorers", "text", "-text", "fusion")

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if got := first(t, stdout); got != "rrf" {
		t.Errorf("first result = %q, want %q", got, "rrf")
	}
}

func TestSearchByVector(t *testing.T) {
	dir := indexed(t)

	code, stdout, stderr := feed(t, "", "search", "-data", dir, "-scorers", "vector", "-vector", "0,1,0")

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if got := first(t, stdout); got != "hnsw" {
		t.Errorf("first result = %q, want %q", got, "hnsw")
	}
}

// TestSearchByQueryString is weft's own query language reaching a command for
// the first time. pkg/query.Parse has existed since milestone 20 and nothing
// outside a Go program could call it.
func TestSearchByQueryString(t *testing.T) {
	dir := indexed(t)

	code, stdout, stderr := feed(t, "", "search", "-data", dir, "-q", "+fusion")

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if got := first(t, stdout); got != "rrf" {
		t.Errorf("first result = %q, want %q", got, "rrf")
	}
	// `+` is a requirement, so a document without the term is excluded rather
	// than ranked below one that has it.
	if strings.Contains(stdout, "hnsw") {
		t.Errorf("a required clause did not exclude a document that fails it:\n%s", stdout)
	}
}

// TestSearchReportsABadQueryString. pkg/query refuses to answer a malformed
// query with zero results, and a command in front of it must not soften that
// into an empty ranking.
func TestSearchReportsABadQueryString(t *testing.T) {
	dir := indexed(t)

	code, _, stderr := feed(t, "", "search", "-data", dir, "-q", `"unterminated`)

	if code == 0 {
		t.Fatal("a malformed query string was accepted")
	}
	if strings.TrimSpace(stderr) == "" {
		t.Error("nothing was written to stderr")
	}
}

// TestSearchScopesToAField reaches text.NewField, which nothing outside a Go
// program could call before this command.
func TestSearchScopesToAField(t *testing.T) {
	dir := indexed(t)

	code, stdout, stderr := feed(t, "", "search", "-data", dir,
		"-scorers", "text:title", "-text", "small world")

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if got := first(t, stdout); got != "hnsw" {
		t.Errorf("first result = %q, want %q — the field-scoped scorer did not match", got, "hnsw")
	}
}

// TestSearchBreakdownShowsEveryStream is the demo's display, moved to a command
// and pointed at an index on disk.
func TestSearchBreakdownShowsEveryStream(t *testing.T) {
	dir := indexed(t)

	code, stdout, stderr := feed(t, "", "search", "-data", dir,
		"-scorers", "text,vector", "-text", "fusion", "-vector", "1,0,0", "-breakdown")

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	for _, want := range []string{"text:", "vector:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the breakdown has no %q column:\n%s", want, stdout)
		}
	}
}

// TestSearchWeightsMustMatchTheStreams. A weight list one short is a silent
// re-ranking, because weights attach to positions and every position after the
// missing one shifts.
func TestSearchWeightsMustMatchTheStreams(t *testing.T) {
	dir := indexed(t)

	code, _, stderr := feed(t, "", "search", "-data", dir,
		"-scorers", "text,vector,recency", "-text", "fusion", "-weights", "1,1")

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "3") || !strings.Contains(stderr, "2") {
		t.Errorf("the message does not give both counts:\n%s", stderr)
	}
}

// TestGraphNeedsASeed. graph.New takes a seed scorer; there is no default one to
// invent, and a graph stream with nothing to walk from is empty for a reason the
// caller cannot see from the output.
func TestGraphNeedsASeed(t *testing.T) {
	dir := indexed(t)

	code, _, stderr := feed(t, "", "search", "-data", dir, "-scorers", "graph", "-text", "fusion")

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "seed") {
		t.Errorf("the message does not say a seed is missing:\n%s", stderr)
	}
}

func TestGraphWalksFromTheScorerBeforeIt(t *testing.T) {
	dir := indexed(t)

	code, stdout, stderr := feed(t, "", "search", "-data", dir,
		"-scorers", "text,graph", "-text", "fusion", "-breakdown")

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "graph:") {
		t.Errorf("the graph stream is not in the breakdown:\n%s", stdout)
	}
}

// TestPersonalizedPageRankIsReachable covers graph.NewPPR, NewAdjacency and both
// options in one run — the half of pkg/scorer/graph no command could reach.
func TestPersonalizedPageRankIsReachable(t *testing.T) {
	dir := indexed(t)

	code, stdout, stderr := feed(t, "", "search", "-data", dir,
		"-scorers", "text,graph:ppr", "-text", "fusion",
		"-restart", "0.2", "-precision", "0.001", "-breakdown")

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "ppr") {
		t.Errorf("the PPR stream is not named in the breakdown:\n%s", stdout)
	}
}

// TestGraphOptionsWithoutTheScorerAreRefused. Accepting a restart probability
// and then not using it is the quiet failure this repository refuses everywhere
// else: a caller tunes a number that changes nothing and reads the unchanged
// ranking as the answer.
func TestGraphOptionsWithoutTheScorerAreRefused(t *testing.T) {
	dir := indexed(t)

	code, _, stderr := feed(t, "", "search", "-data", dir,
		"-scorers", "text", "-text", "fusion", "-restart", "0.2")

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "-restart") {
		t.Errorf("the message does not name the flag that would have been ignored:\n%s", stderr)
	}
}

func TestSearchRefusesAnUnknownScorer(t *testing.T) {
	dir := indexed(t)

	code, _, stderr := feed(t, "", "search", "-data", dir, "-scorers", "bm25", "-text", "fusion")

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "bm25") {
		t.Errorf("the rejected name is not quoted back:\n%s", stderr)
	}
}

// TestSearchNeedsSomethingToRank. Neither a query string nor a scorer list means
// no streams at all, and engine.Search with no scorers returns nothing — a
// success that answers a question nobody asked.
func TestSearchNeedsSomethingToRank(t *testing.T) {
	dir := indexed(t)

	code, _, stderr := feed(t, "", "search", "-data", dir, "-text", "fusion")

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "-scorers") || !strings.Contains(stderr, "-q") {
		t.Errorf("the message does not name the two flags that would fix it:\n%s", stderr)
	}
}

// TestRecencyTakesAClock. recency.New reads time.Now and recency.NewAt takes the
// clock as an argument; -now is how a command reaches the second, and pinning it
// is what makes a recency ranking reproducible in a script or a test.
func TestRecencyTakesAClock(t *testing.T) {
	dir := indexed(t)

	code, stdout, stderr := feed(t, "", "search", "-data", dir,
		"-scorers", "recency", "-text", "fusion", "-now", "2026-08-01T00:00:00Z")

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if got := first(t, stdout); got != "rrf" {
		t.Errorf("first result = %q, want %q — rrf is the newest document", got, "rrf")
	}
}

func TestSearchRefusesABadClock(t *testing.T) {
	dir := indexed(t)

	code, _, stderr := feed(t, "", "search", "-data", dir,
		"-scorers", "recency", "-now", "yesterday")

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "yesterday") {
		t.Errorf("the rejected value is not quoted back:\n%s", stderr)
	}
}
