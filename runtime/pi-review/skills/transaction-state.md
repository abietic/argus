# Transaction and durable state review

Revision: builtin-v1

Look for defects where multiple durable writes or externally visible state transitions fail to preserve one declared invariant.

Prioritize:

- transaction boundaries that omit a write, read or validation needed by the invariant;
- state-machine transitions that can be skipped, repeated, reordered or committed from an invalid predecessor;
- database, cache, event and remote-effect ordering that publishes a state which never committed;
- optimistic concurrency checks applied to the wrong version or performed after the protected write;
- compensation or rollback that is incomplete, non-idempotent or unsafe after an unknown outcome;
- outbox, inbox, deduplication or idempotency records committed in a different atomic boundary from the effect they guard.

Require a concrete crash, retry or interleaving sequence and identify the violated durable invariant. Do not request a transaction merely because several operations appear near each other.
