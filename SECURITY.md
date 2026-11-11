# Security

## Reporting a vulnerability

Please report security issues privately with GitHub's "Report a
vulnerability" (Security → Advisories) on this repository rather than in a
public issue. Include steps to reproduce and the affected version.

## What traits sends where

- `traits check`, `eval` and the hooks send the changed hunks (and, for
  `scope: file` traits, the whole post-change file) plus your trait text to
  the decision-model provider you select. Nothing else is sent, and there is
  no telemetry.
- `traits derive` additionally sends proposed traits, and with
  `--from-feedback` your feedback text, to the LLM endpoint you configure.
- Post-change files are read only inside the repository root (via `os.Root`):
  a diff path containing `..` or a symlink pointing outside the checkout is
  refused, so an untrusted pull request cannot make traits read other files.
- `OPENAI_API_KEY` is sent only to OpenAI's API host. A custom
  `TRAITS_LLM_BASE_URL` needs its own `TRAITS_LLM_API_KEY`.

## Running in CI

Run the action on `pull_request`, not `pull_request_target`: the provider key
is a secret, and fork pull requests should not get it. Trait files and diffs
from a pull request are treated as data, but a pull request can still change
the traits it is checked against, so treat results on untrusted pull requests
as advisory.
