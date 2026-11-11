# OpenAI Decisions wire contract

Checked 2026-11-11 against the API reference
(https://developers.openai.com/api/reference/resources/decisions/methods/create)
and the guide (https://developers.openai.com/api/docs/guides/decisions). No live
call was made; the fixtures in `testdata/` are hand-written from the documented
shapes and the guide's illustrative example (damaged / department / severity).

## Confirmed (reference or guide)

- `POST /v1/decisions`, bearer auth. `model` must be `gpt-6-luna`.
- `input`: a string or an array of messages. A user message is
  `{"role": "user", "content": [parts]}` (no `"type": "message"` in the guide's
  example); `input_json` parts are `{"type": "input_json", "data": <json>}` and
  are allowed only in user messages. `input_json` data is limited to 32 nesting
  levels, 10,000 values and 256 KiB (we enforce only the 256 KiB size, as
  `Limits().MaxInputBytes`).
- `questions`: 1 to 400. Each has `type`, `instructions` (string), optional
  `name`. Choice adds `choices` (2 to 255 entries of `{value, description?}`,
  value a string or boolean, unique). Score adds `levels` (2 to 10 entries of
  `{label, description?}`, lowest to highest).
- `prompt_cache_key`: optional string, at most 64 characters.
- Response: `{model, answers, usage}`. `answers` are in question order; each has
  `type` and `name` (string or null).
  - predicate: `probability`.
  - choice: `choice` (string or boolean), `confidence`, `probabilities` as an
    **array** of `{value, probability}`.
  - score: `score` (the probability-weighted average of 0-based level indices,
    so it may fall between levels), `confidence`, `probabilities` as an
    **array** of `{value (0-based level index), label, probability}`.
  - refusal: `{type: "refusal", name}`; other questions may still be answered.
- `usage`: `input_tokens`, `output_tokens`, `total_tokens`,
  `input_tokens_details.{cached_tokens, cache_write_tokens}`,
  `output_tokens_details.reasoning_tokens`.

## Assumed / chosen here

- Question `name` is sent as `provider.Question.Name` unchanged. The reference
  states no character set or length limit for it; if the API rejects names with
  `/` or similar, switch to positional names (q0, ...) as System One does.
- `prompt_cache_key` longer than 64 characters is replaced by its SHA-256 hex
  (64 characters) rather than rejected.
- `probabilities` is also accepted as an object map (`{value-or-label-or-index:
  p}`) in case the live API differs from the reference; anything else is an
  error. Score entries are matched by `label` first, then by `value` index.
- `confidence`'s meaning is undocumented; it is passed through. When absent, the
  probability of the chosen value is used.
- Answers are matched by name when present, else by position; a named answer at
  position i with the wrong name, a duplicate name, or a count mismatch is an
  error (as jev-lint does).
- Choice and score `probabilities` are optional on our side (missing means no
  distribution); the reference lists them on every choice/score answer.
- Retries: no Retry-After/rate-limit semantics are documented for this endpoint;
  we use the generic 429/5xx policy in `provider.PostJSON`.
- Context window for gpt-6-luna is undocumented.
