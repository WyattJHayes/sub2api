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

type EvaluationRun struct {
	ent.Schema
}

func (EvaluationRun) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "evaluation_runs"}}
}

func (EvaluationRun) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("plan_id", uuid.UUID{}).Immutable(),
		field.String("trigger_source").MaxLen(20).Immutable(),
		field.JSON("baseline_ref", map[string]any{}).SchemaType(map[string]string{dialect.Postgres: "jsonb"}).Immutable(),
		field.JSON("candidate_ref", map[string]any{}).SchemaType(map[string]string{dialect.Postgres: "jsonb"}).Immutable(),
		field.String("status").MaxLen(24).Default("pending"),
		field.Float("budget_limit").SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).Immutable(),
		field.Float("reserved_cost").SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).Default(0),
		field.Float("actual_cost").SchemaType(map[string]string{dialect.Postgres: "numeric(20,8)"}).Default(0),
		field.Bool("calibration_mode").Default(true).Immutable(),
		field.Int64("created_by").Optional().Nillable().Immutable(),
		field.Time("started_at").SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).Optional().Nillable(),
		field.Time("finished_at").SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).Optional().Nillable(),
		field.Time("created_at").SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).Default(time.Now).Immutable(),
		field.Time("updated_at").SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).Default(time.Now).UpdateDefault(time.Now),
	}
}

func (EvaluationRun) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("plan", EvaluationPlan.Type).
			Ref("runs").Field("plan_id").Unique().Required().Immutable(),
	}
}

func (EvaluationRun) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("plan_id", "created_at"),
		index.Fields("status", "created_at"),
	}
}
