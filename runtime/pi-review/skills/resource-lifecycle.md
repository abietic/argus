# Resource lifecycle and ownership review

Revision: builtin-v1

Look for defects in acquisition, ownership transfer, use, cancellation and release of bounded or exclusive resources.

Prioritize:

- files, sockets, streams, locks, transactions, goroutines, tasks or subscriptions leaked on a concrete exit path;
- double close, double unlock, use-after-release and cleanup by the wrong owner;
- deferred or asynchronous cleanup running too late, too early or against a reused handle;
- cancellation not propagated to spawned work, or spawned work outliving data and authority it references;
- pool, semaphore and quota permits not returned exactly once;
- cleanup ordering that loses buffered data, masks the primary failure or leaves a resource externally active.

Trace ownership from acquisition to every relevant terminal path. Do not report a leak when the framework, caller or process lifetime demonstrably owns cleanup.
