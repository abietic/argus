# Error and outcome contract review

Revision: builtin-v1

Look for defects where a failure is translated, propagated or observed incorrectly across a call, process or remote boundary.

Prioritize:

- swallowed errors, success returned after partial work and fallback that hides required failure;
- wrapping or remapping that destroys actionable category, retryability, identity or causality;
- timeout, cancellation and transport failure treated as proof that no side effect occurred;
- partial batch results whose failed or unprocessed members are reported as complete;
- retry loops that violate the callee's idempotency or unknown-outcome contract;
- callers and callees disagreeing about sentinel, status, nullable result or exception semantics.

Require the exact failing path and the incorrect externally observed outcome. Do not report wording, logging or generic observability improvements unless they change control flow or a caller-visible contract.
