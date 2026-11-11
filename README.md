# traits

`traits` checks code changes against your project's **traits**: short Markdown files
in `.agents/traits/` that each state one coding preference, why it matters, and where
it applies. It sends the changed hunks to a *decision model* (a model that answers
typed questions with probabilities, not prose) and reports, per finding, a tier:

- **fix**: the model is confident the change breaks the trait (violation probability >= 0.8 by default);
- **review**: it leans towards a violation or cannot decide (>= 0.5, or inconclusive).

The output is JSON, one-line text, Markdown for a coding agent, or SARIF for code scanning.
The same binary runs as a Claude Code or Codex hook, in CI, and from the command line.
It is a single Go binary with one dependency.

## Install

```sh
go install github.com/talbet/traits/cmd/traits@latest
```

or download a binary from the [releases](https://github.com/talbet/traits/releases)
(darwin, linux, windows; amd64, arm64; `checksums.txt` alongside).

## Quick start

Write a trait, `.agents/traits/no-debug-prints.md`:

```markdown
---
name: no-debug-prints
subject: Debugging output left in code.
requirement: Do not leave fmt.Println debugging statements in committed code.
purpose: Debug output pollutes logs and leaks internals.
appliesTo: "*.go"          # optional path globs
when: ["Println"]          # optional regexes over changed lines; skips the model when absent
fix: Remove the print or use the structured logger.   # optional
---
Optional context: examples, references.
```

Then check your changes:

```sh
export OPENAI_API_KEY=...
git diff HEAD | traits check --diff --format summary
traits check --format text src/              # whole files
traits explain no-debug-prints               # the exact questions sent to the model
```

The format is compatible with [crosshatch/traits](https://github.com/crosshatch/traits)
(`name`, `subject`, `requirement`, `applicability`, `purpose`, body as context); the
other fields are optional extensions. Answers are cached in `.agents/traits/.cache/`
(git-ignored).

## Providers

Choose with `--provider` or `TRAITS_PROVIDER` (default `openai`), and `--model` or
`TRAITS_MODEL` (default: the provider's).

| Provider | Default model | Environment |
|---|---|---|
| `openai` | `gpt-6-luna` | `OPENAI_API_KEY` (`OPENAI_BASE_URL` optional) |
| `openrouter` | `~typesafe/jev-latest` | `OPENROUTER_API_KEY` (`OPENROUTER_BASE_URL` optional) |
| `systemone` | none; `--model` required | `TRAITS_SYSTEMONE_URL` (default `http://127.0.0.1:11434`), `TRAITS_SYSTEMONE_API_KEY` (optional) |
| `opencode` | `jev-1.13-free` | `OPENCODE_API_KEY` |
| `cloudflare` | `clef` | `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ACCOUNT_ID` |

Other variables: `TRAITS_DIR` (traits directory, default `.agents/traits` at the
repository root), `TRAITS_HOOK_TIMEOUT` (hook deadline, default `8s`),
`TRAITS_HOOK_DEBUG` (print why a hook stayed silent, on stderr).

## Output formats

`--format json` (default; the full report), `text` (`path:line: tier[trait] interpretation`),
`summary` (Markdown grouped into "Fix" and "Double-check" for an agent to act on), and
`sarif` (SARIF 2.1.0). Results go to stdout; the one-line summary and errors go to stderr.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | checks ran and nothing reached `--fail-on` (default `fix`; also `review`, `never`) |
| 1 | a finding reached `--fail-on`, or a file could not be fully checked |
| 2 | unusable command line, traits directory or provider configuration |
| 3 | the provider failed (unreachable, refused, malformed answer) |
| 130 | interrupted |

`traits hook` and `traits recheck` always exit 0.

## Agent hooks

```sh
traits hook install claude            # dry run: shows the hooks to merge into .claude/settings.json
traits hook install claude --apply    # writes it (backs up the old file first)
traits hook install codex --apply     # .codex/hooks.json; --scope user for ~/.codex or ~/.claude
traits skill install --apply          # .claude/skills/traits/SKILL.md (--agent codex|opencode: .agents/skills)
```

This adds two hooks:

- **PostToolUse** (`Write|Edit|MultiEdit` for Claude Code, `apply_patch` for Codex) runs
  `traits hook claude|codex`. It rebuilds a diff of the edit, checks it, and feeds the
  top findings (`--top`, default 5) back to the agent as `additionalContext`.
- **Stop** runs `traits recheck`. It re-checks the files with open findings from the
  session and records whether each finding was **fixed** or **kept**.

Both fail open. On any error, timeout, missing traits or missing credentials they print
nothing and exit 0. Credentials must be in the agent's environment.

Every `check`, hook and recheck appends to `.agents/traits/.cache/findings.jsonl`.
`traits findings --summary` shows, per trait, how often it was flagged, fixed and kept.
A trait that agents often keep may be noisy and worth rewording.

## Evaluating traits

Put labelled examples next to a trait in `<name>.cases.jsonl`, one JSON object
per line:

```json
{"split":"dev","path":"src/parse.ts","diff":"@@ -1 +1 @@\n-a\n+b\n","expect":"violation"}
{"split":"holdout","path":"src/ok.ts","content":"export const x = 1\n","expect":"compliant"}
```

`expect` is `violation`, `compliant` or `not-applicable`. Then:

```sh
traits eval                                   # dev split, default provider
traits eval --split holdout --fail-under-f1 0.9
traits eval --compare openai:gpt-6-luna,systemone:nimble
traits eval --promote          # dry run: which candidate traits pass holdout
traits eval --promote --yes    # set them to status active
```

It reports precision, recall, F1 and false alarms per trait at both
thresholds, and how many cases the `appliesTo`/`when` gate dropped. Tune
wording on `dev`; judge on `holdout`. Answers are cached, so reruns are free.

## Deriving traits

`traits derive` adds a preference to the collection safely. It checks whether
a proposed trait is one rule or several (several are split by an LLM), then
how it relates to every stored trait: distinct, already covered (verified by
a second check), a clarification of one, or a conflict. It prints a plan and
writes nothing:

```sh
traits derive proposed.md                               # from a trait file
traits derive --from-feedback "don't wrap callbacks in arrows" --diff-file fix.diff
traits derive --approve 3f9c2a1b7d0e                    # apply exactly that plan
traits derive --approve 3f9c2a1b7d0e --resolve old-trait=replace
```

Approval applies the saved plan you were shown and is refused if any trait
changed since. Drafting from feedback and splitting need an
OpenAI-compatible chat model: set `TRAITS_LLM_MODEL` (and `TRAITS_LLM_BASE_URL`,
`TRAITS_LLM_API_KEY`, which falls back to `OPENAI_API_KEY`).

## CI

```yaml
on: pull_request
permissions:
  contents: read
  security-events: write
jobs:
  traits:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - uses: talbet/traits@v0
        with:
          upload-sarif: true      # findings appear in code scanning
          # provider, model, fail-on (fix), format (sarif), base-ref, version (latest)
        env:
          OPENAI_API_KEY: ${{ secrets.OPENAI_API_KEY }}
```

The action downloads the release binary for the runner, runs
`git diff --no-color <base>...HEAD | traits check --diff`, writes `traits.sarif`,
optionally uploads it, and fails on findings at `fail-on`.

## Credits

- [madxmike/gallant](https://github.com/madxmike/gallant) (MIT): CLI conventions,
  provider registry, diff splitter and fuzz targets; some code is adapted from it.
- [crosshatch/traits](https://github.com/crosshatch/traits) (Apache-2.0): the trait
  format, classification and adapted assessment prompt text.
- Ideas from [Haizzz/lentil](https://github.com/Haizzz/lentil) (chunking, SARIF),
  [ckorhonen/jev-lint](https://github.com/ckorhonen/jev-lint) (hooks, fail-open design,
  re-check and findings log) and [almcc/slop-linter](https://github.com/almcc/slop-linter).

See [NOTICE](NOTICE). Licensed under the [MIT License](LICENSE).
