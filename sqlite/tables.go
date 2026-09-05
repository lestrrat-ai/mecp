package sqlite

import (
	"github.com/lestrrat-go/rasql/query"
	"github.com/lestrrat-go/rasql/schema"
)

// The query builder resolves every column reference against these
// descriptions and refuses a name the table does not hold, so a typo becomes a
// build error at process start rather than a SQL error at run time. They
// restate the DDL in migrations.go, which stays the schema of record;
// TestTableDefsMatchMigratedSchema reads the migrated database back and fails
// when the two drift apart.
//
// Only the column names and the key columns matter here. Nothing in this
// package renders DDL from these definitions, so the declared types serve as
// documentation and as the drift test's expectation.
var (
	recordsTable = query.MustTableRef(schema.MustTableDef("records",
		schema.Text("id"),
		schema.Text("kind"),
		schema.Text("subject"),
		schema.Text("normalized_subject"),
		schema.Text("statement"),
		schema.Text("rationale"),
		schema.Text("authority"),
		schema.Text("status"),
		schema.Float("confidence"),
		schema.Text("valid_from"),
		schema.Text("valid_until", schema.Nullable()),
		schema.Text("review_after", schema.Nullable()),
		schema.Text("last_verified_at", schema.Nullable()),
		schema.Text("validation_policy"),
		schema.Text("superseded_by"),
		schema.Text("conflict_group"),
		schema.Text("created_at"),
		schema.Text("updated_at"),
		schema.PrimaryKey("id"),
	))

	recordScopesTable = query.MustTableRef(schema.MustTableDef("record_scopes",
		schema.Text("record_id"),
		schema.Text("principal"),
		schema.Text("org"),
		schema.Text("repository"),
		schema.Text("branch_patterns"),
		schema.Text("path_patterns"),
		schema.Text("task_kinds"),
		schema.Text("conditions"),
		schema.PrimaryKey("record_id"),
	))

	sourcesTable = query.MustTableRef(schema.MustTableDef("sources",
		schema.Text("id"),
		schema.Text("type"),
		schema.Text("locator"),
		schema.Text("revision"),
		schema.Text("content_hash"),
		schema.Text("exact_excerpt"),
		schema.Text("captured_at"),
		schema.Text("validation_policy"),
		schema.PrimaryKey("id"),
	))

	recordSourcesTable = query.MustTableRef(schema.MustTableDef("record_sources",
		schema.Text("record_id"),
		schema.Text("source_id"),
		schema.Integer("position"),
		schema.PrimaryKey("record_id", "source_id"),
	))

	recordRelationshipsTable = query.MustTableRef(schema.MustTableDef("record_relationships",
		schema.Text("from_record_id"),
		schema.Text("to_record_id"),
		schema.Text("kind"),
		schema.PrimaryKey("from_record_id", "to_record_id", "kind"),
	))

	recordTagsTable = query.MustTableRef(schema.MustTableDef("record_tags",
		schema.Text("record_id"),
		schema.Text("tag"),
		schema.PrimaryKey("record_id", "tag"),
	))

	proposalsTable = query.MustTableRef(schema.MustTableDef("proposals",
		schema.Text("id"),
		schema.Text("proposal_key"),
		schema.Text("status"),
		schema.Text("principal_id"),
		schema.Text("client_id"),
		schema.Text("kind"),
		schema.Text("subject"),
		schema.Text("statement"),
		schema.Text("rationale"),
		schema.Text("scope"),
		schema.Text("tags"),
		schema.Text("supersedes_record_ids"),
		schema.Text("created_at"),
		schema.Text("decided_at", schema.Nullable()),
		schema.Text("decided_by"),
		schema.Text("decision_note"),
		schema.Text("result_record_id"),
		schema.PrimaryKey("id"),
		schema.Unique("proposal_key"),
	))

	proposalSourcesTable = query.MustTableRef(schema.MustTableDef("proposal_sources",
		schema.Text("proposal_id"),
		schema.Integer("position"),
		schema.Text("payload"),
		schema.PrimaryKey("proposal_id", "position"),
	))

	auditEventsTable = query.MustTableRef(schema.MustTableDef("audit_events",
		schema.Integer("id"),
		schema.Text("at"),
		schema.Text("principal_id"),
		schema.Text("client_id"),
		schema.Text("operation"),
		schema.Text("payload"),
		schema.PrimaryKey("id"),
	))

	// recordsFTSTable describes the FTS5 virtual table. It carries no primary
	// key because FTS5 declares none, and the builder needs none: the joins
	// here go through record_id, which FTS5 stores as an ordinary UNINDEXED
	// column, rather than through the hidden rowid.
	recordsFTSTable = query.MustTableRef(schema.MustTableDef("records_fts",
		schema.Text("record_id"),
		schema.Text("subject"),
		schema.Text("statement"),
		schema.Text("rationale"),
		schema.Text("tags"),
		schema.Text("evidence"),
	))
)

// Column references reused across statements. Naming them once keeps a column
// spelled in exactly one place in this package.
var (
	recordID           = recordsTable.Column("id")
	recordUpdatedAt    = recordsTable.Column("updated_at")
	scopeRecordID      = recordScopesTable.Column("record_id")
	tagRecordID        = recordTagsTable.Column("record_id")
	tagName            = recordTagsTable.Column("tag")
	sourceID           = sourcesTable.Column("id")
	recordSourceRecID  = recordSourcesTable.Column("record_id")
	recordSourceSrcID  = recordSourcesTable.Column("source_id")
	relationshipFrom   = recordRelationshipsTable.Column("from_record_id")
	relationshipTo     = recordRelationshipsTable.Column("to_record_id")
	relationshipKind   = recordRelationshipsTable.Column("kind")
	ftsRecordID        = recordsFTSTable.Column("record_id")
	proposalID         = proposalsTable.Column("id")
	proposalEvidenceID = proposalSourcesTable.Column("proposal_id")
)

// supersedesKind is the only record_relationships kind this build writes or
// reads. It is a stored value rather than an identifier, so it travels as a
// bound argument like any other.
const supersedesKind = "supersedes"
