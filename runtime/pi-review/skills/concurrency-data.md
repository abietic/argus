# Concurrency and data integrity review

Revision: builtin-v2

Look for defects involving concurrent execution, memory visibility and shared mutable data.

Prioritize:

- races, stale reads, lost updates and unsafe publication;
- inconsistent lock ordering, work performed while holding broad locks and cancellation leaks;
- missing compare-and-set, fencing or generation checks for concurrent owners;
- cache or publication ordering that exposes impossible intermediate state.

Require a plausible interleaving or failure sequence. Do not infer concurrency merely from naming, and do not report missing locks when ownership or immutability already provides safety.
