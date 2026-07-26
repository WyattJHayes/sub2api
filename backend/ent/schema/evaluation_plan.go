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

type EvaluationPlan struct {
	ent.Schema
}

func (EvaluationPlan) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "evaluation_plans"}}
}

func (EvaluationPlan) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.String("name").MaxLen(120),
		field.UUID("dataset_version_id", uuid.UUID{}).Immutable(),
		field.String("trigger_type").MaxLen(20),
		field.String("cron_expression").MaxLen(100).Optional().Nillable(),
		field.JSON("model_matrix", []map[string]any{}).SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.Float("max_run_cost").SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.Float("daily_cost_limit").SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}),
		field.Int("max_concurrency"),
		field.Bool("enabled").Default(true),
		field.Int64("created_by").Immutable(),
		field.Time("created_at").SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).Default(time.Now).Immutable(),
		field.Time("updated_at").SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).Default(time.Now).UpdateDefault(time.Now),
	}
}

func (EvaluationPlan) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("dataset_version", EvaluationDatasetVersion.Type).
			Ref("plans").Field("dataset_version_id").Unique().Required().Immutable(),
		edge.To("runs", EvaluationRun.Type),
	}
}

func (EvaluationPlan) Indexes() []ent.Index {
	return []ent.Index{index.Fields("enabled", "trigger_type")}
}
