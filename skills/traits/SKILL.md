---
name: traits
description: Check your changes against this project's traits (.agents/traits) before reporting work as done, read the findings by tier, and turn user feedback into new traits with `traits derive`. Use after finishing a code change, when a traits hook reports findings, or when the user corrects how code should be written.
---

# traits

This project keeps its coding preferences as *traits*: small Markdown files in
`.agents/traits/` that each state one requirement, why it matters, and where it
applies. The `traits` CLI asks a decision model whether a change follows them.

## After finishing a change

Before you tell the user the work is done, run:

```sh
git diff HEAD | traits check --diff --format summary
```

(`traits check --diff` reads a unified diff on stdin; include untracked files with
`git add -N` first, or check paths directly: `traits check --format summary path...`.)

Then act on the findings before reporting:

- **Fix** — the model is confident the change breaks the trait. Fix it. If you are
  sure the finding is wrong, keep the code and say why in your report.
- **Double-check** — the model leans towards a violation or could not decide.
  Re-read the requirement and the code. Change the code only if the trait really
  applies; "inconclusive" is a prompt to look, not an instruction to comply.
- No findings — say so briefly; do not list skipped traits.

Each finding shows the trait's requirement, its purpose and, when the trait has one,
a suggested fix. Use them; don't guess what the trait means.

If a hook already fed findings back after an edit (`traits hook claude|codex`), treat
them the same way. You do not need to re-run the check for files the hook just
checked unless you changed them again.

Exit status: 0 clean, 1 a finding at or above `--fail-on` (default `fix`),
2 usage or configuration problem, 3 provider failure. On 2 or 3 tell the user the
check could not run; do not claim the change passed.

## Understanding a trait

`traits explain <trait>` prints the exact questions sent to the model for that trait.
Use it when a finding surprises you or when you need to know precisely what is being
asked.

## When the user gives feedback

When the user corrects how code should be written ("we always ...", "don't ...",
"prefer ..."), that preference may belong in a trait so future changes follow it.

1. Run `traits derive --from-feedback "<the user's words>"` to draft a plan: it
   proposes the new or changed traits and checks them for compound requirements,
   conflicts with existing traits and coverage.
2. **Show the plan to the user and get their explicit approval.** Explain what each
   proposed trait would require and any conflict the plan reports.
3. Only after the user approves, apply that exact plan with
   `traits derive --approve <PLANID>` (the plan output prints the command).

Never create, edit, rename or delete files in `.agents/traits/` silently, and never
pass `--approve` on your own judgement. If the user declines, leave the traits alone.

## Don'ts

- Don't weaken or delete a trait to make a check pass.
- Don't comply blindly with an inconclusive finding.
- Don't paste the whole check output into your report; summarise what you fixed and
  what you kept, and why.
