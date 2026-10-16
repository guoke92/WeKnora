package sqldb

import (
	"context"
	"database/sql"
	"fmt"
	"hash/crc32"
	"net"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/Tencent/WeKnora/internal/utils"
)

// pgDialect reads pg_catalog and samples rows with TABLESAMPLE where possible.
type pgDialect struct {
	db *sql.DB
}

// openPostgres opens a pool for one sync run. Every connection dials through the
// SSRF-safe dialer and runs with default_transaction_read_only=on, so the server
// refuses writes even if a statement bypassed the read-only transaction wrapper.
func openPostgres(ctx context.Context, cfg config) (dialect, error) {
	pc, err := pgx.ParseConfig(pgDSN(cfg))
	if err != nil {
		return nil, fmt.Errorf("invalid PostgreSQL settings: %w", err)
	}
	pc.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return utils.SSRFSafeDialContext(ctx, network, addr)
	}
	pc.RuntimeParams["default_transaction_read_only"] = "on"
	pc.RuntimeParams["application_name"] = "weknora-datasource"
	pc.RuntimeParams["statement_timeout"] = strconv.FormatInt(cfg.queryTimeout.Milliseconds(), 10)

	db := stdlib.OpenDB(*pc)
	db.SetMaxOpenConns(2)
	return &pgDialect{db: db}, nil
}

// pgDSN renders a keyword/value connection string. Values are single-quoted so
// spaces and special characters in the password cannot change the parameters.
func pgDSN(cfg config) string {
	quote := func(v string) string {
		return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
	}
	sslmode := cfg.tls
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=10",
		quote(cfg.host), cfg.port, quote(cfg.user), quote(cfg.password), quote(cfg.database), sslmode)
}

func (d *pgDialect) close() error { return d.db.Close() }

func (d *pgDialect) ping(ctx context.Context) error {
	return readOnlyPing(ctx, d.db)
}

func (d *pgDialect) listSchemas(ctx context.Context) ([]string, error) {
	rows, err := readRows(ctx, d.db, `
SELECT nspname FROM pg_namespace
WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'
ORDER BY nspname`, 1)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, asString(r[0]))
	}
	return out, nil
}

func (d *pgDialect) listTables(ctx context.Context, schema string) ([]tableRef, error) {
	rows, err := readRows(ctx, d.db, `
SELECT c.relname, COALESCE(obj_description(c.oid, 'pg_class'), ''), c.reltuples::bigint
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relkind IN ('r', 'p')
ORDER BY c.relname`, 3, schema)
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

// tableOID is a sub-select resolving schema.table to its oid; placeholders $1 and $2.
const tableOID = `(SELECT c.oid FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2)`

func (d *pgDialect) describe(ctx context.Context, schema, table string) (*tableMeta, error) {
	meta := &tableMeta{Schema: schema, Name: table, RowEstimate: -1}

	info, err := readRows(ctx, d.db, `
SELECT COALESCE(obj_description(c.oid, 'pg_class'), ''), c.reltuples::bigint
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2`, 2, schema, table)
	if err != nil {
		return nil, err
	}
	if len(info) != 1 {
		return nil, fmt.Errorf("table %s.%s not found", schema, table)
	}
	meta.Comment = asString(info[0][0])
	meta.RowEstimate = asInt64(info[0][1])

	colRows, err := readRows(ctx, d.db, `
SELECT a.attname, format_type(a.atttypid, a.atttypmod), NOT a.attnotnull,
       COALESCE(pg_get_expr(ad.adbin, ad.adrelid), ''), COALESCE(col_description(a.attrelid, a.attnum), '')
FROM pg_attribute a
LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
WHERE a.attrelid = `+tableOID+` AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`, 5, schema, table)
	if err != nil {
		return nil, err
	}
	for _, r := range colRows {
		typ := strings.ToLower(asString(r[1]))
		meta.Columns = append(meta.Columns, columnMeta{
			Name:       asString(r[0]),
			DataType:   typ,
			ColumnType: typ,
			Nullable:   r[2] == true,
			Default:    asString(r[3]),
			Comment:    asString(r[4]),
		})
	}

	idxRows, err := readRows(ctx, d.db, `
SELECT ic.relname, i.indisprimary, i.indisunique, a.attname
FROM pg_index i
JOIN pg_class ic ON ic.oid = i.indexrelid
CROSS JOIN LATERAL unnest(i.indkey::int2[]) WITH ORDINALITY AS k(attnum, ord)
JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
WHERE i.indrelid = `+tableOID+`
ORDER BY ic.relname, k.ord`, 4, schema, table)
	if err != nil {
		return nil, err
	}
	indexes := map[string]*indexMeta{}
	var order []string
	primary := map[string]bool{}
	for _, r := range idxRows {
		name := asString(r[0])
		idx, ok := indexes[name]
		if !ok {
			idx = &indexMeta{Name: name, Unique: r[2] == true}
			indexes[name] = idx
			order = append(order, name)
			primary[name] = r[1] == true
		}
		idx.Columns = append(idx.Columns, asString(r[3]))
	}
	for _, name := range order {
		if primary[name] {
			meta.PrimaryKey = indexes[name].Columns
			continue
		}
		meta.Indexes = append(meta.Indexes, *indexes[name])
	}

	fkRows, err := readRows(ctx, d.db, `
SELECT con.conname, ca.attname, rn.nspname, rc.relname, ra.attname
FROM pg_constraint con
JOIN pg_class rc ON rc.oid = con.confrelid
JOIN pg_namespace rn ON rn.oid = rc.relnamespace
CROSS JOIN LATERAL unnest(con.conkey, con.confkey) WITH ORDINALITY AS k(ck, rk, ord)
JOIN pg_attribute ca ON ca.attrelid = con.conrelid AND ca.attnum = k.ck
JOIN pg_attribute ra ON ra.attrelid = con.confrelid AND ra.attnum = k.rk
WHERE con.conrelid = `+tableOID+` AND con.contype = 'f'
ORDER BY con.conname, k.ord`, 5, schema, table)
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

func (d *pgDialect) quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func (d *pgDialect) textOf(col string) string   { return "CAST(" + col + " AS text)" }
func (d *pgDialect) lengthOf(col string) string { return "octet_length(CAST(" + col + " AS text))" }

// sampleClause uses a block-level TABLESAMPLE sized to roughly three times the sample
// budget so the sample spreads across the table instead of the physical head. The
// seed is derived from the table name, so unchanged tables yield the same sample and
// their rendered documents stay byte-identical. Tables without statistics fall back
// to the physical head, which the report labels as such.
func (d *pgDialect) sampleClause(meta *tableMeta, limit int) (string, string, string) {
	tail := fmt.Sprintf("LIMIT %d", limit)
	est := meta.RowEstimate
	switch {
	case est < 0:
		return "", tail, "head (no planner statistics; run ANALYZE for a spread sample)"
	case est <= int64(limit)*2:
		return "", tail, "whole table (within sample size)"
	default:
		pct := 100.0 * float64(limit) * 3 / float64(est)
		if pct > 100 {
			pct = 100
		}
		seed := crc32.ChecksumIEEE([]byte(meta.Schema + "." + meta.Name))
		return fmt.Sprintf(" TABLESAMPLE SYSTEM (%.6f) REPEATABLE (%d)", pct, seed),
			tail, "block sample (TABLESAMPLE SYSTEM)"
	}
}

func (d *pgDialect) queryRows(ctx context.Context, query string, width int) ([][]interface{}, error) {
	return readRows(ctx, d.db, query, width)
}
