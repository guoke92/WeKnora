package sqldb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
)

// RunQuery checks sqlText and runs it inside a READ ONLY transaction against the data
// source described by cfg. Only tables that the last sync extracted may be referenced.
// Results are capped by rows and bytes: the query is stopped when a cap is reached.
func (c *Connector) RunQuery(
	ctx context.Context, ds *types.DataSourceConfig, cursor *types.SyncCursor,
	sqlText string, limits types.ExternalQueryLimits,
) (*types.ExternalQueryResult, error) {
	cfg, err := parseConfig(c.engine, ds)
	if err != nil {
		return nil, err
	}
	cur, err := decodeCursor(cursor, c.engine, cfg.target())
	if err != nil {
		return nil, err
	}
	if len(cur.Tables) == 0 {
		return nil, errors.New("该数据源尚未同步出任何表，无法查询")
	}

	var refs []tableKey
	switch c.engine {
	case engineMySQL:
		refs, err = checkMySQLSelect(sqlText, cfg.database)
	case enginePostgres:
		refs, err = checkPostgresSelect(sqlText)
	default:
		err = fmt.Errorf("%w: engine %q does not support queries", datasource.ErrInvalidConfig, c.engine)
	}
	if err != nil {
		return nil, err
	}
	extracted := extractedTables(cur)
	for _, ref := range refs {
		if !extracted[ref] {
			return nil, fmt.Errorf("表 %s.%s 不在该数据源已提取的表中", ref.Schema, ref.Table)
		}
	}

	if limits.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limits.Timeout)
		defer cancel()
	}
	d, err := c.open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.close() }()
	db, err := dialectDB(d)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	res, err := queryLimited(ctx, db, sqlText, limits)
	if err != nil {
		return nil, err
	}
	res.Duration = time.Since(start)
	return res, nil
}

// extractedTables lists the tables recorded in the cursor. Keys are the JSON-encoded
// [schema, table] pairs written by the sync.
func extractedTables(cur cursor) map[tableKey]bool {
	out := make(map[tableKey]bool, len(cur.Tables))
	for id := range cur.Tables {
		var parts []string
		if err := json.Unmarshal([]byte(id), &parts); err != nil || len(parts) != 2 {
			continue
		}
		out[tableKey{Schema: parts[0], Table: parts[1]}] = true
	}
	return out
}

// dialectDB returns the connection pool behind a dialect.
func dialectDB(d dialect) (*sql.DB, error) {
	switch v := d.(type) {
	case *mysqlDialect:
		return v.db, nil
	case *pgDialect:
		return v.db, nil
	}
	return nil, fmt.Errorf("dialect %T has no query connection", d)
}

// queryLimited runs the statement and reads rows until a limit is reached. A row that
// would exceed MaxBytes is not returned, and the result is marked truncated.
func queryLimited(ctx context.Context, db *sql.DB, sqlText string, limits types.ExternalQueryLimits) (*types.ExternalQueryResult, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, sqlText)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	res := &types.ExternalQueryResult{Columns: cols, Rows: [][]string{}}
	values := make([]interface{}, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}
	for rows.Next() {
		if len(res.Rows) >= limits.MaxRows {
			res.Truncated = true
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make([]string, len(cols))
		size := 0
		for i, v := range values {
			row[i] = cellText(v)
			size += len(row[i])
		}
		if res.Bytes+size > limits.MaxBytes {
			res.Truncated = true
			break
		}
		res.Bytes += size
		res.Rows = append(res.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return res, nil
}

// cellText renders one driver value as text for the result table.
func cellText(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(x)
	case time.Time:
		return x.Format(time.RFC3339Nano)
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}
