# OpenRouter Decisions wire contract

Checked 2026-11-11. No live call was made; `testdata/` fixtures are
hand-written from the sources below. **Choice and score answer shapes are not
verified against a recorded response.**

## Sources

1. OpenRouter cookbook "Gate Agent Tool Calls with Jev"
   (https://openrouter.ai/docs/cookbook/building-agents/gate-tool-calls-with-jev.md):
   `POST https://openrouter.ai/api/alpha/decisions`, `Authorization: Bearer
   $OPENROUTER_API_KEY`, body `{model, state, questions}`, model
   `typesafe/jev-1.13`, questions keyed by name with `{type: "noul",
   instructions}`, answers keyed by name `{type: "noul", noul: 0..1}`, response
   also carries `id`, `provider`, `usage.cost`.
2. OpenRouter Go SDK reference (client-sdks/go/sdks/decisions,
   models/components/decisions{response,choiceanswer,scoreanswer,refusalanswer}):
   question types noul (criteria `{true, false}`), choice (criteria map of
   option to description), score (ordered criteria list), box; answers are a map
   of `DecisionsResponseAnswers` = box | choice | noul | refusal | score. Choice
   answer: `choice` string, `confidence?`, `probabilities?` map[string]float64.
   Score answer: `score` float64, `confidence?`, `probabilities?`
   map[string]float64, `legend?`. Refusal: only `type` ("The provider declined
   to answer this question; no value or probabilities are returned"). Response:
   `answers`, `model`, `usage` (required), `id?`, `provider?`, `annotations?`.
3. `@effect/ai-openrouter@4.1.2` (inspected read-only from the npm tarball;
   `src/OpenRouterClient.ts`, `OpenRouterSchema.ts`,
   `OpenRouterDecisionModel.ts`): posts to `{apiUrl without trailing /v1}/alpha/decisions`
   (default apiUrl `https://openrouter.ai/api/v1`); state may be a string,
   object or array (sent as a JSON value); score criteria are level
   descriptions; score `probabilities` keys are level indices as strings
   (`answer.probabilities[String(index)]`); probabilities "arrive rounded to two
   decimals"; `usage.{input_tokens, output_tokens, cost?}`.

## Confirmed

Endpoint, auth, body envelope, question map keyed by caller ids, noul
question/answer shape, answer map keyed by the same ids.

## Supported by SDK types only (not by a recorded response)

- Choice/score answers and their `probabilities` maps (choice keyed by option
  value; score keyed by 0-based level index).
- The meaning of score `score`: the Effect adapter passes it through as the
  rating. We compute `Answer.Score` as the probability-weighted level index from
  the distribution when present (as for every System One server), and fall back
  to the reported `score` otherwise.
- Refusal answer `type` value `"refusal"` (inferred from the component name).

## Assumed / chosen here

- Positional ids `q0..` are used as question keys (as for System One).
- `OPENROUTER_BASE_URL` overrides the base; a trailing `/v1` is stripped.
- `usage` is treated as optional; only `input_tokens` is read (no cached-token
  field is documented).
- Default model `~typesafe/jev-latest` (from traits.sh); bare ids such as
  `jev-latest` are mapped onto `typesafe/` by OpenRouter per its SystemOne SDK
  page. No model list is declared.
- No question-count or input-size limit is documented.
