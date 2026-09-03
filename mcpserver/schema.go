package mcpserver

import (
	"encoding/json"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/lestrrat-ai/mecp"
)

// The input schemas are written out rather than inferred from Go structs
// because the constraints that matter here — closed objects, enumerated task
// kinds, bounded budgets and limits — are what keep a model from sending the
// server something surprising. Schema inference cannot express them.

func ptr[T any](v T) *T { return &v }

// noAdditionalProperties is the JSON Schema `false` used to close an object.
func noAdditionalProperties() *jsonschema.Schema {
	return &jsonschema.Schema{Not: &jsonschema.Schema{}}
}

func enumOf[T ~string](values []T) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		out = append(out, string(v))
	}
	return out
}

func mustDefault(v any) json.RawMessage {
	buf, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return buf
}

func workspaceSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:                 "object",
		Description:          "Where the work is happening. Supply as much as the host knows; the server canonicalizes the repository.",
		AdditionalProperties: noAdditionalProperties(),
		Properties: map[string]*jsonschema.Schema{
			"root_uri": {
				Type:        "string",
				Description: "Absolute file:// URI of the workspace root.",
				MaxLength:   ptr(4096),
			},
			"repository": {
				Type:        "string",
				Description: "Git remote URL. SSH and HTTPS spellings resolve to the same repository.",
				MaxLength:   ptr(2048),
			},
			"revision": {
				Type:        "string",
				Description: "Current commit. Used to detect records that describe a different revision.",
				MaxLength:   ptr(256),
			},
			"branch": {
				Type:      "string",
				MaxLength: ptr(512),
			},
			"relevant_paths": {
				Type:        "array",
				Description: "Repository-relative paths the task touches.",
				MaxItems:    ptr(128),
				Items:       &jsonschema.Schema{Type: "string", MaxLength: ptr(2048)},
			},
		},
	}
}

// conditionsSchema appears only where a caller supplies conditions, never where
// one is written into a record's scope. A condition matches only when the
// caller passes it, so a record scoped to one nothing supplies can never be
// returned, and offering the field on a write invites exactly that.
func conditionsSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:                 "object",
		Description:          "Facts about this call that conditional records are matched against. Only what you pass here can match.",
		AdditionalProperties: &jsonschema.Schema{Type: "string", MaxLength: ptr(256)},
		MaxProperties:        ptr(16),
	}
}

func prepareTaskSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:                 "object",
		AdditionalProperties: noAdditionalProperties(),
		Required:             []string{"task"},
		Properties: map[string]*jsonschema.Schema{
			"task": {
				Type:        "string",
				Description: "The concrete task about to be planned or executed, in the user's own words where possible.",
				MinLength:   ptr(1),
				MaxLength:   ptr(20000),
			},
			"task_kind": {
				Type:        "string",
				Description: "What kind of work this is. Records can be scoped to particular kinds. Omit it rather than guessing: an omitted kind applies task-scoped records, a wrong one hides them.",
				Enum:        enumOf(mecp.AllTaskKinds),
			},
			"workspace":  workspaceSchema(),
			"conditions": conditionsSchema(),
			"token_budget": {
				Type:        "integer",
				Description: "Approximate token budget for the returned context pack.",
				Minimum:     ptr(float64(mecp.MinimumTokenBudget)),
				Maximum:     ptr(float64(12000)),
				Default:     mustDefault(mecp.DefaultTokenBudget),
			},
			"include_evidence_summaries": {
				Type:        "boolean",
				Description: "Include a one-line description of what backs each record. Verbatim evidence is fetched separately.",
				Default:     mustDefault(false),
			},
		},
	}
}

func searchSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:                 "object",
		AdditionalProperties: noAdditionalProperties(),
		Required:             []string{"query"},
		Properties: map[string]*jsonschema.Schema{
			"query": {
				Type:        "string",
				Description: "The targeted follow-up question.",
				MinLength:   ptr(1),
				MaxLength:   ptr(4000),
			},
			"context_id": {
				Type:        "string",
				Description: "Handle returned by a previous prepare_task call. Supply this or a workspace.",
				MaxLength:   ptr(128),
			},
			"workspace":  workspaceSchema(),
			"conditions": conditionsSchema(),
			"task_kind": {
				Type: "string",
				Enum: enumOf(mecp.AllTaskKinds),
			},
			"kinds": {
				Type:        "array",
				Description: "Restrict results to these record kinds.",
				MaxItems:    ptr(len(mecp.AllRecordKinds)),
				Items:       &jsonschema.Schema{Type: "string", Enum: enumOf(mecp.AllRecordKinds)},
			},
			"include_stale": {
				Type:        "boolean",
				Description: "Also return superseded and archived records, marked as history.",
				Default:     mustDefault(false),
			},
			"limit": {
				Type:    "integer",
				Minimum: ptr(float64(1)),
				Maximum: ptr(float64(50)),
				Default: mustDefault(8),
			},
		},
	}
}

func getRecordsSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:                 "object",
		AdditionalProperties: noAdditionalProperties(),
		Required:             []string{"record_ids"},
		Properties: map[string]*jsonschema.Schema{
			"record_ids": {
				Type:        "array",
				Description: "Record IDs previously returned by this server.",
				MinItems:    ptr(1),
				MaxItems:    ptr(64),
				Items:       &jsonschema.Schema{Type: "string", MaxLength: ptr(128)},
			},
			"include_evidence": {
				Type:        "boolean",
				Description: "Include verbatim source excerpts where this client is permitted to see them.",
				Default:     mustDefault(false),
			},
			"max_evidence_characters_per_record": {
				Type:    "integer",
				Minimum: ptr(float64(1)),
				Maximum: ptr(float64(20000)),
				Default: mustDefault(2000),
			},
		},
	}
}

func proposeRecordSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:                 "object",
		AdditionalProperties: noAdditionalProperties(),
		Required:             []string{"proposal_key", "kind", "statement"},
		Properties: map[string]*jsonschema.Schema{
			"proposal_key": {
				Type:        "string",
				Description: "Stable key for this suggestion. Repeating it returns the existing proposal instead of creating a duplicate.",
				MinLength:   ptr(1),
				MaxLength:   ptr(256),
			},
			"kind": {
				Type: "string",
				Enum: enumOf(mecp.AllRecordKinds),
			},
			"subject": {
				Type:        "string",
				Description: "Short name for what the record is about.",
				MaxLength:   ptr(256),
			},
			"statement": {
				Type:        "string",
				Description: "The normalized assertion, written as the user would state it.",
				MinLength:   ptr(1),
				MaxLength:   ptr(8000),
			},
			"rationale": {
				Type:      "string",
				MaxLength: ptr(8000),
			},
			"scope": {
				Type:                 "object",
				Description:          "Where the proposed record applies. Prefer the narrowest scope that is actually true.",
				AdditionalProperties: noAdditionalProperties(),
				Properties: map[string]*jsonschema.Schema{
					"repository":      {Type: "string", MaxLength: ptr(2048)},
					"branch_patterns": {Type: "array", MaxItems: ptr(32), Items: &jsonschema.Schema{Type: "string", MaxLength: ptr(512)}},
					"path_patterns":   {Type: "array", MaxItems: ptr(64), Items: &jsonschema.Schema{Type: "string", MaxLength: ptr(2048)}},
					"task_kinds":      {Type: "array", MaxItems: ptr(len(mecp.AllTaskKinds)), Items: &jsonschema.Schema{Type: "string", Enum: enumOf(mecp.AllTaskKinds)}},
				},
			},
			"tags": {
				Type:     "array",
				MaxItems: ptr(16),
				Items:    &jsonschema.Schema{Type: "string", MaxLength: ptr(64)},
			},
			"evidence": {
				Type:        "array",
				Description: "What supports the proposal. Quote the source rather than paraphrasing it.",
				MaxItems:    ptr(16),
				Items: &jsonschema.Schema{
					Type:                 "object",
					AdditionalProperties: noAdditionalProperties(),
					Required:             []string{"locator"},
					Properties: map[string]*jsonschema.Schema{
						"type":          {Type: "string", Enum: enumOf(mecp.AllSourceTypes)},
						"locator":       {Type: "string", MinLength: ptr(1), MaxLength: ptr(2048)},
						"revision":      {Type: "string", MaxLength: ptr(256)},
						"exact_excerpt": {Type: "string", MaxLength: ptr(8000)},
					},
				},
			},
			"supersedes_record_ids": {
				Type:     "array",
				MaxItems: ptr(16),
				Items:    &jsonschema.Schema{Type: "string", MaxLength: ptr(128)},
			},
		},
	}
}

func extractRulesSchema() *jsonschema.Schema {
	scopeSchema := func(desc string) *jsonschema.Schema {
		return &jsonschema.Schema{
			Type:                 "object",
			Description:          desc,
			AdditionalProperties: noAdditionalProperties(),
			Properties: map[string]*jsonschema.Schema{
				"repository":      {Type: "string", MaxLength: ptr(2048)},
				"branch_patterns": {Type: "array", MaxItems: ptr(32), Items: &jsonschema.Schema{Type: "string", MaxLength: ptr(512)}},
				"path_patterns":   {Type: "array", MaxItems: ptr(64), Items: &jsonschema.Schema{Type: "string", MaxLength: ptr(2048)}},
				"task_kinds":      {Type: "array", MaxItems: ptr(len(mecp.AllTaskKinds)), Items: &jsonschema.Schema{Type: "string", Enum: enumOf(mecp.AllTaskKinds)}},
			},
		}
	}

	return &jsonschema.Schema{
		Type:                 "object",
		AdditionalProperties: noAdditionalProperties(),
		Required:             []string{"document_path", "rules"},
		Properties: map[string]*jsonschema.Schema{
			"document_path": {
				Type:        "string",
				Description: "The instruction file the rules were read from. It must be inside a configured document root.",
				MinLength:   ptr(1),
				MaxLength:   ptr(4096),
			},
			"scope": scopeSchema("Applies to every rule that does not carry its own. One document usually covers one area."),
			"rules": {
				Type:        "array",
				Description: "The rules found in the document, in the order they appear.",
				MinItems:    ptr(1),
				MaxItems:    ptr(200),
				Items: &jsonschema.Schema{
					Type:                 "object",
					AdditionalProperties: noAdditionalProperties(),
					Required:             []string{"kind", "statement", "quote"},
					Properties: map[string]*jsonschema.Schema{
						"kind": {
							Type:        "string",
							Description: "constraint for an absolute rule, preference for a default, and the other kinds where they fit.",
							Enum:        enumOf(mecp.AllRecordKinds),
						},
						"subject": {
							Type:        "string",
							Description: "What the rule is about, usually the heading it sits under.",
							MaxLength:   ptr(256),
						},
						"statement": {
							Type:        "string",
							Description: "The rule as one self-contained sentence, understandable without the document.",
							MinLength:   ptr(1),
							MaxLength:   ptr(4000),
						},
						"rationale": {
							Type:        "string",
							Description: "Why the rule holds, when the document says so. Do not invent one.",
							MaxLength:   ptr(4000),
						},
						"quote": {
							Type: "string",
							Description: "The exact text this rule came from, copied from the document. " +
								"The server checks it against the file and refuses the rule if it does not appear, " +
								"so it must be copied rather than paraphrased.",
							MinLength: ptr(1),
							MaxLength: ptr(4000),
						},
						"tags":  {Type: "array", MaxItems: ptr(16), Items: &jsonschema.Schema{Type: "string", MaxLength: ptr(64)}},
						"scope": scopeSchema("Overrides the document-wide scope for this one rule."),
					},
				},
			},
		},
	}
}
