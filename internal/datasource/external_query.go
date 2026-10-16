package datasource

import (
	"context"

	"github.com/Tencent/WeKnora/internal/types"
)

// ExternalQuerier is implemented by connectors that can run checked, read-only SQL
// against the database they extract. cursor is the data source's last sync cursor: it
// lists the tables that may be referenced, and it must belong to the same database as
// cfg. Connectors that do not implement it cannot be queried by the agent.
type ExternalQuerier interface {
	RunQuery(
		ctx context.Context, cfg *types.DataSourceConfig, cursor *types.SyncCursor,
		sql string, limits types.ExternalQueryLimits,
	) (*types.ExternalQueryResult, error)
}
