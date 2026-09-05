// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"
)

// exercise runs the CLI the way main does, with the streams captured.
func exercise(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errs bytes.Buffer
	code = run(args, strings.NewReader(""), &out, &errs)
	return code, out.String(), errs.String()
}

// TestNoArgumentsPrintsTheHelp pins what replaced the demo.
//
// `weft` with no arguments used to index a built-in corpus and read queries from
// stdin. It is a command with subcommands now, and the argument-less form is the
// one a reader types first — so it prints the help rather than starting a session
// whose prompt says nothing about `index` or `search`. It exits 2 because no work
// was requested, which is what cmd/weft-eval does with the same input.
func TestNoArgumentsPrintsTheHelp(t *testing.T) {
	code, stdout, stderr := exercise(t)

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "usage: weft") {
		t.Errorf("stderr has no usage line:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("nothing was asked for, so stdout should be empty; got:\n%s", stdout)
	}
}

// TestExplicitHelpIsNotAnError separates the two ways help gets printed. Asking
// for it is a request that succeeded; being handed it after typing nothing is a
// diagnostic. A reader piping `weft help` into a pager should not be told the
// command failed.
func TestExplicitHelpIsNotAnError(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			code, stdout, stderr := exercise(t, arg)

			if code != 0 {
				t.Errorf("exit code = %d, want 0", code)
			}
			if !strings.Contains(stdout, "usage: weft") {
				t.Errorf("stdout has no usage line:\n%s", stdout)
			}
			if stderr != "" {
				t.Errorf("help was asked for, so stderr should be empty; got:\n%s", stderr)
			}
		})
	}
}

// TestUnknownSubcommandIsNamed keeps the typo in the message. "unknown
// subcommand" alone sends a reader looking at the whole line for what was wrong
// with it.
func TestUnknownSubcommandIsNamed(t *testing.T) {
	code, _, stderr := exercise(t, "serach")

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, `"serach"`) {
		t.Errorf("the rejected name is not quoted back:\n%s", stderr)
	}
	if !strings.Contains(stderr, "usage: weft") {
		t.Errorf("stderr has no usage line:\n%s", stderr)
	}
}

// TestUsageListsEverySubcommand is the drift check between the two places that
// have to agree: the switch that dispatches a name, and the text that tells a
// reader the name exists. A subcommand missing from the help is a subcommand
// nobody runs, and one in the help that nothing dispatches is worse.
func TestUsageListsEverySubcommand(t *testing.T) {
	var out bytes.Buffer
	usage(&out)
	help := out.String()

	for _, name := range subcommands {
		if !strings.Contains(help, name) {
			t.Errorf("subcommand %q dispatches but the help never mentions it", name)
		}
		code, _, stderr := exercise(t, name, "-h")
		if code == 2 && strings.Contains(stderr, "unknown subcommand") {
			t.Errorf("the help lists %q but nothing dispatches it", name)
		}
	}
}
