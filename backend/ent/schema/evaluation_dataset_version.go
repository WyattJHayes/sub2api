package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

type EvaluationDatasetVersion struct {
	ent.Schema
}

func (EvaluationDatasetVersion) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "evaluation_dataset_versions"}}
}

func (EvaluationDatasetVersion) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.String("dataset_key").MaxLen(100).Immutable(),
		field.String("version").MaxLen(100).Immutable(),
		field.String("manifest_sha256").MaxLen(64).Immutable(),
		field.String("source_type").MaxLen(20).Immutable(),
		field.String("status").MaxLen(20).Default("draft"),
		field.Int64("created_by").Immutable(),
		field.Time("published_at").SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).Optional().Nillable(),
		field.Time("retired_at").SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).Optional().Nillable(),
		field.Time("created_at").SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).Default(time.Now).Immutable(),
		field.Time("updated_at").SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).Default(time.Now).UpdateDefault(time.Now),
	}
}

func (EvaluationDatasetVersion) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("cases", EvaluationCase.Type),
		edge.To("plans", EvaluationPlan.Type),
	}
}

func (EvaluationDatasetVersion) Indexes() []ent.Index { return nil }
