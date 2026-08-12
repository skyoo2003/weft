# Pull Request

## Summary

<!--
Briefly describe the purpose and context of this PR.

Consider including:
- What problem does this PR solve?
- Why is this change necessary?
- What behavior changes after this PR?

Example:
Request validation logic was duplicated across multiple packages.
This PR extracts the shared validation logic into a reusable package
to reduce duplication and improve maintainability.
-->

## Changes

<!--
Describe the main changes in terms of behavior or functionality.
Focus on "what changed" rather than listing modified files.

Example:
- Added a shared validation package
- Removed duplicated validation logic from handlers
- Standardized error wrapping and propagation
- Added unit tests for the new validation behavior
-->

-
-
-

## Validation

<!--
Describe how the changes were verified.

Include relevant commands, test coverage, or manual verification when applicable.

Examples:
- `go test ./...`
- `go vet ./...`
- Verified key scenarios locally
- Confirmed no regressions in existing behavior
- For concurrency-related changes: `go test -race ./...`
-->

-

## Review Focus

<!--
Highlight areas that deserve extra attention during review.
Use "None" if there is nothing specific to call out.

Examples:
- Verify goroutine lifecycle and context cancellation
- Check error wrapping and propagation for consistency
- Confirm backward compatibility of public APIs
- Check for potential races or deadlocks around mutex/channel usage
-->

-

## Risks / Notes

<!--
Document risks, constraints, operational considerations, or follow-up work
that may not be obvious from the code itself.

Use "None" if there are no notable risks or additional notes.

Examples:
- No changes to existing public interfaces
- A new configuration value has been introduced
- A database migration is required before deployment
- A deprecated API will be removed in a follow-up PR
-->

-
