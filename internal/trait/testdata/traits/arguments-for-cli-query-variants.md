---
name: arguments-for-cli-query-variants
subject: CLI query commands that differ only in the kind of data selected.
requirement: Prefer one query command with a validated selector argument over
  separate thin command wrappers around a shared command factory when options
  and behavior are otherwise the same. Keep separate commands when their
  semantics or interfaces meaningfully differ.
purpose: Keep the CLI and its implementation simple without redundant command
  indirection.
---
