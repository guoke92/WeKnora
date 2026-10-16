package sqldb

import (
	"context"
	"database/sql"
	"fmt"
)

// dialect is the engine-specific surface of the connector. Every query issued through
// it is read-only: sessions are opened inside a READ ONLY transaction, so even a
// mistaken statement cannot change data.
type dialect interface {
	ping(ctx context.Context) error
	listSchemas(ctx context.Context) ([]string, error)
	listTables(ctx context.Context, schema string) ([]tableRef, error)
	describe(ctx context.Context, schema, table string) (*tableMeta, error)

	// quoteIdent escapes an identifier for the engine.
	quoteIdent(name string) string
	// textOf returns an expression that yields the column as text.
	textOf(col string) string
	// lengthOf returns an expression yielding the length of the column's value.
	lengthOf(col string) string
	// sampleClause returns extra FROM-clause text (for example TABLESAMPLE) and the
	// tail (ORDER BY / LIMIT). mode describes the sampling strategy for the report.
	sampleClause(meta *tableMeta, limit int) (fromExtra, tail, mode string)

	// queryRows runs a read-only query and returns every row as driver values.
	queryRows(ctx context.Context, query string, width int) ([][]interface{}, error)

	close() error
}

// opener creates a dialect for one sync run. The caller closes it.
type opener func(ctx context.Context, cfg config) (dialect, error)

// tableRef is one entry of a schema listing.
type tableRef struct {
	Name        string
	Comment     string
	RowEstimate int64 // negative when the engine has no estimate
}

// tableMeta is the structural description of one table.
type tableMeta struct {
	Schema      string
	Name        string
	Comment     string
	RowEstimate int64
	Columns     []columnMeta
	PrimaryKey  []string
	Indexes     []indexMeta
	ForeignKeys []foreignKeyMeta
}

type columnMeta struct {
	Name       string
	DataType   string // engine data type, used for classification (may include parameters)
	ColumnType string // full display type, e.g. varchar(64)
	Nullable   bool
	Default    string
	Comment    string
}

type indexMeta struct {
	Name    string
	Unique  bool
	Columns []string
}

type foreignKeyMeta struct {
	Name       string
	Columns    []string
	RefSchema  string
	RefTable   string
	RefColumns []string
}

// readRows executes a query inside a READ ONLY transaction and scans every row.
func readRows(ctx context.Context, db *sql.DB, query string, width int, args ...interface{}) ([][]interface{}, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out [][]interface{}
	for rows.Next() {
		values := make([]interface{}, width)
		ptrs := make([]interface{}, width)
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, values)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// readOnlyPing verifies that a read-only session can be opened and used.
func readOnlyPing(ctx context.Context, db *sql.DB) error {
	rows, err := readRows(ctx, db, "SELECT 1", 1)
	if err != nil {
		return err
	}
	if len(rows) != 1 {
		return fmt.Errorf("unexpected ping result")
	}
	return nil
}

// asString converts a driver value to text. Nil becomes "".
func asString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	case int64:
		return fmt.Sprintf("%d", t)
	default:
		return fmt.Sprint(t)
	}
}

// asInt64 converts a driver numeric value. Unknown values yield -1.
func asInt64(v interface{}) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int32:
		return int64(t)
	case int:
		return int64(t)
	case uint64:
		// BIGINT UNSIGNED (for example information_schema.TABLES.TABLE_ROWS) arrives as uint64.
		if t > 1<<63-1 {
			return -1
		}
		return int64(t)
	case uint32:
		return int64(t)
	case float64:
		return int64(t)
	case []byte:
		var n int64
		if _, err := fmt.Sscanf(string(t), "%d", &n); err == nil {
			return n
		}
	case string:
		var n int64
		if _, err := fmt.Sscanf(t, "%d", &n); err == nil {
			return n
		}
	}
	return -1
}
