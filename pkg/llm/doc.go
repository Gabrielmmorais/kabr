// Package llm refines a baseline DomainPlan with a large language model,
// using ONLY catalog metadata.
//
// The graph segmenter (pkg/segment) produces a deterministic baseline plan.
// A [Refiner] asks a model to propose a [plan.Refinement] on top of it:
// domain renames and descriptions, table moves between domains and sensitive
// column annotations. The refinement is parsed strictly by [ParseResponse]
// and later applied defensively by plan.Apply, which validates every item
// against the catalog; this package never trusts the model to respect the
// catalog.
//
// # Privacy invariant
//
// [BuildPrompt] builds the user message from an explicit metadata-only
// structure, never by marshaling schema types directly. What is sent:
//
//   - table IDs ("schema.table") and table comments;
//   - column names, data types, nullability and column comments;
//   - primary key columns, foreign keys (columns, referenced table,
//     referenced columns) and unique constraint columns;
//   - the baseline plan: domain names, and per table its ID, role and shared
//     flag.
//
// What is never sent: column default expressions (they may embed literal
// values), planner statistics (row estimates, n_distinct), index
// definitions, constraint names, the database name, the introspection
// timestamp, connection information, baseline descriptions/rationales and,
// of course, row data (Kabr never reads rows). The system prompt is a
// constant with no catalog interpolation. Tests walk every key of the user
// document against an allowlist and plant sentinel values in the forbidden
// fields.
//
// # Providers
//
//   - "anthropic": the official SDK (github.com/anthropics/anthropic-sdk-go/v2).
//     Credentials come from the SDK's own chain (ANTHROPIC_API_KEY,
//     ANTHROPIC_AUTH_TOKEN, profiles, ...); this package never reads or logs
//     them. Default model "claude-opus-5-5", effort "medium", max tokens
//     16000. The answer is constrained with structured outputs
//     (output_config.format = json_schema with [ResponseSchema]). Thinking
//     is left unset: the model uses adaptive thinking by default.
//   - "ollama": a local Ollama server via net/http (POST /api/chat with
//     "format" set to [ResponseSchema], stream false, temperature 0).
//     Default model "llama3.1", base URL from Config.BaseURL, KABR_OLLAMA_URL
//     or http://127.0.0.1:11434. Effort is ignored.
//
// KABR_LLM_MODEL overrides the default model of either provider when
// Config.Model is empty.
//
// The request is deliberately simple: one model, no server-side fallbacks
// and no beta features, so evaluation runs are reproducible against a single
// known model. A refusal ([ErrRefused]) or truncated answer ([ErrTruncated])
// is returned as an error; the planner then keeps the graph baseline.
package llm
