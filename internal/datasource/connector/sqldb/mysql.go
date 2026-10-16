package sqldb

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/Tencent/WeKnora/internal/utils"
)

// mysqlDialect reads MySQL information_schema and samples rows.
type mysqlDialect struct {
	db       *sql.DB
	database string
}

// openMySQL opens a pool dedicated to one sync run. Connections dial through the
// SSRF-safe dialer so a hostname resolving to a private address is rejected.
func openMySQL(_ context.Context, cfg config) (dialect, error) {
	utils.RegisterMySQLSSRFDialer()

	mc := mysql.NewConfig()
	mc.User = cfg.user
	mc.Passwd = cfg.password
	mc.Net = utils.MySQLSSRFNetwork
	mc.Addr = net.JoinHostPort(cfg.host, strconv.Itoa(cfg.port))
	mc.DBName = cfg.database
	mc.ParseTime = true
	mc.Timeout = 10 * time.Second
	mc.ReadTimeout = cfg.queryTimeout
	mc.WriteTimeout = cfg.queryTimeout
	mc.AllowCleartextPasswords = false
	switch cfg.tls {
	case tlsDisable:
		mc.TLSConfig = "false"
	case tlsVerifyFull:
		mc.TLSConfig = "true"
	default:
		mc.TLSConfig = "skip-verify"
	}

	db, err := sql.Open("mysql", mc.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetConnMaxLifetime(10 * time.Minute)
	return &mysqlDialect{db: db, database: cfg.database}, nil
}

func (d *mysqlDialect) close() error { return d.db.Close() }

func (d *mysqlDialect) ping(ctx context.Context) error {
	if err := readOnlyPing(ctx, d.db); err != nil {
		return err
	}
	rows, err := readRows(ctx, d.db,
		"SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", 1, d.database)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("database %q not found or not accessible", d.database)
	}
	return nil
}

func (d *mysqlDialect) listSchemas(ctx context.Context) ([]string, error) {
	// MySQL "schema" is the database itself; only the configured database is exposed.
	if err := d.ping(ctx); err != nil {
		return nil, err
	}
	return []string{d.database}, nil
}

func (d *mysqlDialect) listTables(ctx context.Context, schema string) ([]tableRef, error) {
	rows, err := readRows(ctx, d.db, `
SELECT TABLE_NAME, COALESCE(TABLE_COMMENT, ''), TABLE_ROWS
FROM information_schema.TABLES
WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE'
ORDER BY TABLE_NAME`, 3, schema)
	if err != nil {
		return nil, err
	}
	out := make([]tableRef, 0, len(rows))
	for _, r := range rows {
		out = append(out, tableRef{
			Name:        asString(r[0]),
			Comment:     asString(r[1]),
			RowEstimate: asInt64(r[2]),
		})
	}
	return out, nil
}

func (d *mysqlDialect) describe(ctx context.Context, schema, table string) (*tableMeta, error) {
	meta := &tableMeta{Schema: schema, Name: table, RowEstimate: -1}

	tableRows, err := readRows(ctx, d.db, `
SELECT COALESCE(TABLE_COMMENT, ''), TABLE_ROWS
FROM information_schema.TABLES
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`, 2, schema, table)
	if err != nil {
		return nil, err
	}
	if len(tableRows) != 1 {
		return nil, fmt.Errorf("table %s.%s not found", schema, table)
	}
	meta.Comment = asString(tableRows[0][0])
	meta.RowEstimate = asInt64(tableRows[0][1])

	colRows, err := readRows(ctx, d.db, `
SELECT COLUMN_NAME, DATA_TYPE, COLUMN_TYPE, IS_NULLABLE, COALESCE(COLUMN_DEFAULT, ''), COALESCE(COLUMN_COMMENT, '')
FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
ORDER BY ORDINAL_POSITION`, 6, schema, table)
	if err != nil {
		return nil, err
	}
	for _, r := range colRows {
		meta.Columns = append(meta.Columns, columnMeta{
			Name:       asString(r[0]),
			DataType:   strings.ToLower(asString(r[1])),
			ColumnType: asString(r[2]),
			Nullable:   strings.EqualFold(asString(r[3]), "YES"),
			Default:    asString(r[4]),
			Comment:    asString(r[5]),
		})
	}

	idxRows, err := readRows(ctx, d.db, `
SELECT INDEX_NAME, NON_UNIQUE, COLUMN_NAME
FROM information_schema.STATISTICS
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
ORDER BY INDEX_NAME, SEQ_IN_INDEX`, 3, schema, table)
	if err != nil {
		return nil, err
	}
	indexes := map[string]*indexMeta{}
	var order []string
	for _, r := range idxRows {
		name := asString(r[0])
		idx, ok := indexes[name]
		if !ok {
			idx = &indexMeta{Name: name, Unique: asInt64(r[1]) == 0}
			indexes[name] = idx
			order = append(order, name)
		}
		idx.Columns = append(idx.Columns, asString(r[2]))
	}
	for _, name := range order {
		idx := indexes[name]
		if name == "PRIMARY" {
			meta.PrimaryKey = idx.Columns
			continue
		}
		meta.Indexes = append(meta.Indexes, *idx)
	}

	fkRows, err := readRows(ctx, d.db, `
SELECT CONSTRAINT_NAME, COLUMN_NAME, REFERENCED_TABLE_SCHEMA, REFERENCED_TABLE_NAME, REFERENCED_COLUMN_NAME
FROM information_schema.KEY_COLUMN_USAGE
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND REFERENCED_TABLE_NAME IS NOT NULL
ORDER BY CONSTRAINT_NAME, ORDINAL_POSITION`, 5, schema, table)
	if err != nil {
		return nil, err
	}
	fks := map[string]*foreignKeyMeta{}
	var fkOrder []string
	for _, r := range fkRows {
		name := asString(r[0])
		fk, ok := fks[name]
		if !ok {
			fk = &foreignKeyMeta{Name: name, RefSchema: asString(r[2]), RefTable: asString(r[3])}
			fks[name] = fk
			fkOrder = append(fkOrder, name)
		}
		fk.Columns = append(fk.Columns, asString(r[1]))
		fk.RefColumns = append(fk.RefColumns, asString(r[4]))
	}
	for _, name := range fkOrder {
		meta.ForeignKeys = append(meta.ForeignKeys, *fks[name])
	}
	return meta, nil
}

func (d *mysqlDialect) quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func (d *mysqlDialect) textOf(col string) string   { return "CAST(" + col + " AS CHAR)" }
func (d *mysqlDialect) lengthOf(col string) string { return "LENGTH(" + col + ")" }

// sampleClause prefers the newest rows by primary key (InnoDB clusters on the PK, so
// this is cheap). Without a primary key it falls back to an unordered head sample,
// which the report marks as biased.
func (d *mysqlDialect) sampleClause(meta *tableMeta, limit int) (string, string, string) {
	if len(meta.PrimaryKey) == 0 {
		return "", fmt.Sprintf("LIMIT %d", limit), "head (no primary key)"
	}
	cols := make([]string, 0, len(meta.PrimaryKey))
	for _, c := range meta.PrimaryKey {
		cols = append(cols, d.quoteIdent(c)+" DESC")
	}
	return "", fmt.Sprintf("ORDER BY %s LIMIT %d", strings.Join(cols, ", "), limit), "latest by primary key"
}

func (d *mysqlDialect) queryRows(ctx context.Context, query string, width int) ([][]interface{}, error) {
	return readRows(ctx, d.db, query, width)
}
