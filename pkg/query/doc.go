// SPDX-License-Identifier: Apache-2.0

// Package query is the constraints rank fusion does not express on its own.
//
// # The trap this package exists to close
//
// Rank fusion is a *union of votes*. fusion.Fuse scores a document by summing
// over the streams it appears in, so being absent from one stream costs it
// nothing in the streams where it is present. A scorer that returns only the
// documents satisfying some condition — an exact phrase, a term pattern, a price
// ceiling — therefore expresses a **ranking preference and not a filter**, and
// everything it refused comes back on another scorer's vote with no error.
//
// docs/ADOPTION.md section 8 is a trial subject running into that and losing
// time to it, and engine.Search's Fuser parameter exists so the fix is writable
// from outside. This package is that fix written once:
//
//	// "matches the phrase, and is not a draft"
//	fuse := query.Must(query.MustNot(fusion.Fuse, 2), 1)
//	results, err := engine.Search(ctx, q, 10, fuse, txt, phrase, drafts)
//
// # Two rules a caller has to hold, because nothing here can
//
// **A restricting scorer must return every match, not its top k.** Search asks
// each scorer for k and a restriction truncated to k excludes every document
// below its own cut — which is a wrong answer rather than a narrow one. The
// scorers in this package return everything they match and say so; a scorer of
// your own used as a restriction has to do the same, which means not calling
// engine.TopK on the way out.
//
// **Stream indexes are positions in the argument list you already fixed.**
// Must(fuse, 1) restricts on the second scorer passed to Search. That is the
// same convention fusion.FuseWeighted uses and for the same reason: the caller
// fixed the order, so nothing here has to learn what a scorer is. Nothing in
// this package imports a scorer, and `go list -deps ./pkg/query` names none.
//
// # What is here and what is not
//
// Glob covers prefix, suffix and wildcard term queries. Phrase decides an exact
// phrase from the document text, because a Posting carries a frequency and not a
// position — docs/FORMAT.md section 8 prices the index that would change that.
// Must and MustNot are the boolean shell.
//
// Not here: edit-distance (fuzzy) matching, field restriction, numeric ranges,
// and a string query syntax. The first three want index support this format does
// not have; the fourth is a parser over the above and is worth writing once
// there is something stable to parse into.
package query
