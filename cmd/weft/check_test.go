// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckPassesACleanIndex. engine.Scrub verifies a directory rather than
// changing it, and a command in front of it exists so that "is this index
// intact" is a question with an answer that does not require writing a program.
func TestCheckPassesACleanIndex(t *testing.T) {
	dir := indexed(t)

	code, stdout, stderr := feed(t, "", "check", "-data", dir)

	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, dir) {
		t.Errorf("the report does not name what was checked:\n%s", stdout)
	}
}

// TestCheckReportsDamage. The whole value of a checker is the failing case: one
// that passes a truncated segment is worse than no checker at all, because it is
// a clean bill of health over a corpus that cannot be read.
func TestCheckReportsDamage(t *testing.T) {
	dir := indexed(t)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var damaged string
	for _, e := range entries {
		if !e.IsDir() && strings.Contains(e.Name(), "seg") {
			damaged = filepath.Join(dir, e.Name())
			break
		}
	}
	if damaged == "" {
		t.Skipf("no segment file to damage in %v", entries)
	}
	if err := os.WriteFile(damaged, []byte("not a segment"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	code, _, stderr := feed(t, "", "check", "-data", dir)

	if code == 0 {
		t.Fatalf("a damaged index passed the check; stderr:\n%s", stderr)
	}
	if strings.TrimSpace(stderr) == "" {
		t.Error("nothing was written to stderr")
	}
}

func TestCheckNeedsADirectory(t *testing.T) {
	code, _, stderr := feed(t, "", "check")

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "-data") {
		t.Errorf("the message does not name the missing flag:\n%s", stderr)
	}
}

// TestEncodeInt is what makes a numeric range query writable by hand.
// query.Range compares bytes, so a bound has to be written in the encoding the
// indexing side used — pkg/query says exactly that, and until now saying it was
// all a caller got.
func TestEncodeInt(t *testing.T) {
	var encoded []string
	for _, v := range []string{"-9", "0", "42"} {
		code, stdout, stderr := feed(t, "", "encode", "-int", v)
		if code != 0 {
			t.Fatalf("encode -int %s: exit %d, stderr:\n%s", v, code, stderr)
		}
		encoded = append(encoded, strings.TrimSpace(stdout))
	}

	// The property the encoding exists for: byte order is numeric order, so a
	// range query over encoded bounds means what a reader thinks it means.
	if encoded[0] >= encoded[1] || encoded[1] >= encoded[2] {
		t.Errorf("encoded order does not follow numeric order: %q", encoded)
	}
}

func TestEncodeTime(t *testing.T) {
	early, late := "2020-01-01T00:00:00Z", "2026-09-06T00:00:00Z"

	_, a, _ := feed(t, "", "encode", "-time", early)
	code, b, stderr := feed(t, "", "encode", "-time", late)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if strings.TrimSpace(a) >= strings.TrimSpace(b) {
		t.Errorf("the earlier time did not encode below the later one: %q vs %q", a, b)
	}
}

func TestEncodeRefusesABadTime(t *testing.T) {
	code, _, stderr := feed(t, "", "encode", "-time", "last tuesday")

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "last tuesday") {
		t.Errorf("the rejected value is not quoted back:\n%s", stderr)
	}
}

// TestEncodeNeedsExactlyOne. Both at once has no single answer to print, and
// neither is a command that would print an empty line and exit 0.
func TestEncodeNeedsExactlyOne(t *testing.T) {
	t.Run("neither", func(t *testing.T) {
		code, _, stderr := feed(t, "", "encode")
		if code != 2 {
			t.Errorf("exit code = %d, want 2", code)
		}
		if !strings.Contains(stderr, "-int") || !strings.Contains(stderr, "-time") {
			t.Errorf("the message does not name both flags:\n%s", stderr)
		}
	})
	t.Run("both", func(t *testing.T) {
		code, _, stderr := feed(t, "", "encode", "-int", "1", "-time", "2026-01-01T00:00:00Z")
		if code != 2 {
			t.Errorf("exit code = %d, want 2", code)
		}
		if strings.TrimSpace(stderr) == "" {
			t.Error("nothing was written to stderr")
		}
	})
}
