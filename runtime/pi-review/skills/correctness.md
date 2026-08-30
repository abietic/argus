# Correctness and reliability review

Revision: builtin-v2

Look for defects that can make the changed code produce a wrong result or fail under realistic inputs.

Prioritize:

- violated preconditions, boundary conditions, off-by-one behavior and incorrect defaults;
- incorrect branching, termination, fallback or state-machine transitions;
- incompatibilities between a changed caller and callee contract;
- missing handling for nil, empty, duplicate, malformed or partially available data.

Leave error semantics, resource ownership and durable transaction atomicity to their dedicated dimensions unless they directly prove a wrong computation or control-flow result.

Only report an issue when you can explain a concrete triggering path and user-visible or system-visible impact. Do not report style preferences, speculative refactors or generic hardening advice.
