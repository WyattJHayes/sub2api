package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

type EvaluationCase struct {
	ent.Schema
}

func (EvaluationCase) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "evaluation_cases"}}
}

func (EvaluationCase) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("dataset_version_id", uuid.UUID{}).Immutable(),
		field.String("case_key").MaxLen(160).Immutable(),
		field.String("capability_domain").MaxLen(32).Immutable(),
		field.String("priority").MaxLen(4).Immutable(),
		field.Float("weight").SchemaType(map[string]string{dialect.Postgres: "numeric(10,4)"}).Immutable(),
		field.Int("sample_count").Immutable(),
		field.JSON("prompt_spec", map[string]any{}).SchemaType(map[string]string{dialect.Postgres: "jsonb"}).Optional(),
		field.JSON("expected_spec", map[string]any{}).SchemaType(map[string]string{dialect.Postgres: "jsonb"}).Optional(),
		field.String("encrypted_spec").SchemaType(map[string]string{dialect.Postgres: "text"}).Optional().Nillable(),
		field.JSON("execution_spec", map[string]any{}).SchemaType(map[string]string{dialect.Postgres: "jsonb"}).Immutable(),
		field.String("grader_id").MaxLen(100).Immutable(),
		field.String("grader_version").MaxLen(100).Immutable(),
		field.String("content_sha256").MaxLen(64).Immutable(),
		field.String("confidentiality").MaxLen(20).Immutable(),
		field.Float("estimated_cost").SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).Default(0).Immutable(),
		field.Time("created_at").SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).Default(time.Now).Immutable(),
	}
}

func (EvaluationCase) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("dataset_version", EvaluationDatasetVersion.Type).
			Ref("cases").Field("dataset_version_id").Unique().Required().Immutable(),
	}
}

func (EvaluationCase) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("dataset_version_id", "case_key").Unique(),
		index.Fields("dataset_version_id", "capability_domain", "priority"),
	}
}
