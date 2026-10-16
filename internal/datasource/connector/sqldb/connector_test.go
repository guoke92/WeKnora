package sqldb

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
)

// fakeDialect serves canned metadata so sync semantics can be tested without a server.
type fakeDialect struct {
	schemas     []string
	tables      map[string][]tableRef
	metas       map[string]*tableMeta
	describeErr map[string]error
	listErr     error
	// samples holds sampled rows by "schema.table", keyed by column name.
	samples map[string][]map[string]interface{}
}

func (f *fakeDialect) ping(context.Context) error { return nil }
func (f *fakeDialect) close() error               { return nil }

func (f *fakeDialect) listSchemas(context.Context) ([]string, error) {
	return append([]string(nil), f.schemas...), nil
}

func (f *fakeDialect) listTables(_ context.Context, schema string) ([]tableRef, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]tableRef(nil), f.tables[schema]...), nil
}

func (f *fakeDialect) describe(_ context.Context, schema, table string) (*tableMeta, error) {
	key := schema + "." + table
	if err := f.describeErr[key]; err != nil {
		return nil, err
	}
	meta, ok := f.metas[key]
	if !ok {
		return nil, errors.New("not found")
	}
	cp := *meta
	return &cp, nil
}

func (f *fakeDialect) quoteIdent(s string) string { return "`" + s + "`" }
func (f *fakeDialect) textOf(c string) string     { return c }
func (f *fakeDialect) lengthOf(c string) string   { return "LENGTH(" + c + ")" }
func (f *fakeDialect) sampleClause(*tableMeta, int) (string, string, string) {
	return "", "LIMIT 10", "fake"
}

// queryRows serves the sample of the table named in the query, laid out the way the
// profile expects it.
func (f *fakeDialect) queryRows(_ context.Context, query string, _ int) ([][]interface{}, error) {
	for key, rows := range f.samples {
		meta, ok := f.metas[key]
		if !ok || !strings.Contains(query, "`"+meta.Schema+"`.`"+meta.Name+"`") {
			continue
		}
		plans := planColumns(meta, config{})
		spec := buildSpec(f, meta, plans, 0)
		out := make([][]interface{}, 0, len(rows))
		for _, values := range rows {
			out = append(out, makeRow(plans, meta, values, spec.width))
		}
		return out, nil
	}
	return nil, nil
}

// recHandler records what the connector emitted and checkpointed.
type recHandler struct {
	items       []types.FetchedItem
	checkpoints int
	last        *types.SyncCursor
}

func (h *recHandler) Emit(_ context.Context, item types.FetchedItem) error {
	h.items = append(h.items, item)
	return nil
}

func (h *recHandler) Checkpoint(_ context.Context, c *types.SyncCursor) error {
	h.checkpoints++
	h.last = c
	return nil
}

func (h *recHandler) emittedIDs() []string {
	var ids []string
	for _, it := range h.items {
		if !it.IsDeleted && it.Metadata["error"] == "" {
			ids = append(ids, it.ExternalID)
		}
	}
	sort.Strings(ids)
	return ids
}

func (h *recHandler) deletedIDs() []string {
	var ids []string
	for _, it := range h.items {
		if it.IsDeleted {
			ids = append(ids, it.ExternalID)
		}
	}
	sort.Strings(ids)
	return ids
}

func (h *recHandler) errorItems() []types.FetchedItem {
	var out []types.FetchedItem
	for _, it := range h.items {
		if it.Metadata["error"] != "" {
			out = append(out, it)
		}
	}
	return out
}

func newFakeShop() *fakeDialect {
	orders := &tableMeta{
		Schema: "shop", Name: "orders", RowEstimate: 1200,
		Columns: []columnMeta{
			{Name: "id", DataType: "bigint", ColumnType: "bigint"},
			{Name: "status", DataType: "varchar", ColumnType: "varchar(16)"},
		},
		PrimaryKey: []string{"id"},
	}
	users := &tableMeta{
		Schema: "shop", Name: "users", RowEstimate: 50,
		Columns: []columnMeta{
			{Name: "id", DataType: "bigint", ColumnType: "bigint"},
			{Name: "name", DataType: "varchar", ColumnType: "varchar(64)"},
		},
		PrimaryKey: []string{"id"},
	}
	return &fakeDialect{
		schemas: []string{"shop", "analytics"},
		tables: map[string][]tableRef{
			"shop":      {{Name: "orders", RowEstimate: 1200}, {Name: "users", RowEstimate: 50}},
			"analytics": {{Name: "events", RowEstimate: 10}},
		},
		metas: map[string]*tableMeta{
			"shop.orders": orders,
			"shop.users":  users,
			"analytics.events": {
				Schema: "analytics", Name: "events", RowEstimate: 10,
				Columns:    []columnMeta{{Name: "id", DataType: "bigint", ColumnType: "bigint"}},
				PrimaryKey: []string{"id"},
			},
		},
	}
}

func newTestConnector(d *fakeDialect) *Connector {
	return &Connector{engine: engineMySQL, open: func(context.Context, config) (dialect, error) { return d, nil }}
}

func shopDS(resourceIDs ...string) *types.DataSourceConfig {
	return &types.DataSourceConfig{
		Credentials: map[string]interface{}{"user": "reader", "password": "secret"},
		Settings: map[string]interface{}{
			"host":     "db.internal",
			"database": "shop",
			"tls_mode": "disable",
		},
		ResourceIDs: resourceIDs,
	}
}

func runOnce(t *testing.T, c *Connector, ds *types.DataSourceConfig, old *types.SyncCursor, full bool) (*recHandler, *types.SyncCursor) {
	t.Helper()
	h := &recHandler{}
	var (
		next *types.SyncCursor
		err  error
	)
	if full {
		next, err = c.FetchFullStream(context.Background(), ds, old, h)
	} else {
		next, err = c.FetchStream(context.Background(), ds, old, h)
	}
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	return h, next
}

func TestFirstSyncEmitsEveryTableDocument(t *testing.T) {
	d := newFakeShop()
	ds := shopDS(encodeSchemaID("shop"))
	h, next := runOnce(t, newTestConnector(d), ds, nil, false)

	want := []string{encodeTableID("shop", "orders"), encodeTableID("shop", "users")}
	if got := h.emittedIDs(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("emitted %v, want %v", got, want)
	}
	if len(h.deletedIDs()) != 0 {
		t.Fatalf("unexpected deletions on first sync: %v", h.deletedIDs())
	}
	for _, it := range h.items {
		if it.ContentType != "text/markdown" || !strings.HasPrefix(string(it.Content), "# shop.") {
			t.Fatalf("unexpected document for %s: %q", it.ExternalID, it.Title)
		}
		if it.Metadata["channel"] != types.ChannelMySQL {
			t.Fatalf("channel = %q, want mysql", it.Metadata["channel"])
		}
	}
	if h.checkpoints == 0 {
		t.Fatalf("expected at least one checkpoint")
	}
	if n := len(next.ConnectorCursor["tables"].(map[string]interface{})); n != 2 {
		t.Fatalf("cursor tracks %d tables, want 2", n)
	}
}

func TestUnchangedTablesAreNotReemitted(t *testing.T) {
	d := newFakeShop()
	ds := shopDS(encodeSchemaID("shop"))
	c := newTestConnector(d)
	_, cur := runOnce(t, c, ds, nil, false)

	h, _ := runOnce(t, c, ds, cur, false)
	if got := h.emittedIDs(); len(got) != 0 {
		t.Fatalf("unchanged run emitted %v", got)
	}
	if len(h.deletedIDs()) != 0 {
		t.Fatalf("unchanged run deleted %v", h.deletedIDs())
	}
}

func TestChangedTableIsReemittedOnly(t *testing.T) {
	d := newFakeShop()
	ds := shopDS(encodeSchemaID("shop"))
	c := newTestConnector(d)
	_, cur := runOnce(t, c, ds, nil, false)

	d.metas["shop.orders"].Columns = append(d.metas["shop.orders"].Columns,
		columnMeta{Name: "paid_at", DataType: "datetime", ColumnType: "datetime"})
	h, _ := runOnce(t, c, ds, cur, false)
	if got := h.emittedIDs(); len(got) != 1 || got[0] != encodeTableID("shop", "orders") {
		t.Fatalf("emitted %v, want only orders", got)
	}
}

func TestTableLeavingSelectionIsDeleted(t *testing.T) {
	d := newFakeShop()
	c := newTestConnector(d)
	_, cur := runOnce(t, c, shopDS(encodeSchemaID("shop")), nil, false)

	// Narrow the selection to orders only; users must be deleted.
	h, next := runOnce(t, c, shopDS(encodeTableID("shop", "orders")), cur, false)
	if got := h.deletedIDs(); len(got) != 1 || got[0] != encodeTableID("shop", "users") {
		t.Fatalf("deleted %v, want users", got)
	}
	if _, ok := next.ConnectorCursor["tables"].(map[string]interface{})[encodeTableID("shop", "users")]; ok {
		t.Fatalf("deleted table still tracked in cursor")
	}
}

func TestDroppedTableIsDeleted(t *testing.T) {
	d := newFakeShop()
	c := newTestConnector(d)
	_, cur := runOnce(t, c, shopDS(encodeSchemaID("shop")), nil, false)

	d.tables["shop"] = []tableRef{{Name: "orders", RowEstimate: 1200}}
	delete(d.metas, "shop.users")
	h, _ := runOnce(t, c, shopDS(encodeSchemaID("shop")), cur, false)
	if got := h.deletedIDs(); len(got) != 1 || got[0] != encodeTableID("shop", "users") {
		t.Fatalf("deleted %v, want users", got)
	}
}

func TestForceFullReemitsEverything(t *testing.T) {
	d := newFakeShop()
	c := newTestConnector(d)
	ds := shopDS(encodeSchemaID("shop"))
	_, cur := runOnce(t, c, ds, nil, false)

	h, _ := runOnce(t, c, ds, cur, true)
	if got := h.emittedIDs(); len(got) != 2 {
		t.Fatalf("full sync emitted %v, want both tables", got)
	}
	if len(h.deletedIDs()) != 0 {
		t.Fatalf("full sync deleted %v", h.deletedIDs())
	}
}

func TestDescribeFailureKeepsPreviousDocument(t *testing.T) {
	d := newFakeShop()
	c := newTestConnector(d)
	ds := shopDS(encodeSchemaID("shop"))
	_, cur := runOnce(t, c, ds, nil, false)

	d.describeErr = map[string]error{"shop.orders": errors.New("lock wait timeout for host db.internal")}
	h, next := runOnce(t, c, ds, cur, false)

	errs := h.errorItems()
	if len(errs) != 1 {
		t.Fatalf("error items = %d, want 1", len(errs))
	}
	if errs[0].ExternalID != encodeTableID("shop", "orders") {
		t.Fatalf("error item id = %q", errs[0].ExternalID)
	}
	if strings.Contains(errs[0].Metadata["error"], "db.internal") {
		t.Fatalf("driver detail leaked into sync log: %q", errs[0].Metadata["error"])
	}
	if len(h.deletedIDs()) != 0 {
		t.Fatalf("failed table must not be deleted, got %v", h.deletedIDs())
	}
	fp := next.ConnectorCursor["tables"].(map[string]interface{})[encodeTableID("shop", "orders")]
	if fp == nil || fp == "" {
		t.Fatalf("failed table lost its fingerprint")
	}
}

func TestMaxTablesOverflowKeepsSkippedDocuments(t *testing.T) {
	d := newFakeShop()
	c := newTestConnector(d)
	ds := shopDS(encodeSchemaID("shop"))
	ds.Settings["max_tables"] = float64(1)
	_, cur := runOnce(t, c, ds, nil, false)
	// Pretend users was synced earlier; overflow must not delete it.
	cur.ConnectorCursor["tables"].(map[string]interface{})[encodeTableID("shop", "users")] = "old-fp"

	h, next := runOnce(t, c, ds, cur, false)
	errs := h.errorItems()
	if len(errs) != 1 || errs[0].Metadata["error_reason_code"] != "max_tables_exceeded" {
		t.Fatalf("expected max_tables_exceeded error item, got %+v", errs)
	}
	if len(h.deletedIDs()) != 0 {
		t.Fatalf("skipped table deleted: %v", h.deletedIDs())
	}
	tables := next.ConnectorCursor["tables"].(map[string]interface{})
	if tables[encodeTableID("shop", "users")] != "old-fp" {
		t.Fatalf("skipped table fingerprint changed")
	}
}

func TestCursorFromAnotherDatabaseIsRejected(t *testing.T) {
	d := newFakeShop()
	c := newTestConnector(d)
	ds := shopDS(encodeSchemaID("shop"))
	_, cur := runOnce(t, c, ds, nil, false)

	other := shopDS(encodeSchemaID("shop"))
	other.Settings["host"] = "another.internal"
	_, err := c.FetchStream(context.Background(), other, cur, &recHandler{})
	if !errors.Is(err, datasource.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

func TestSyncRequiresSelection(t *testing.T) {
	c := newTestConnector(newFakeShop())
	_, err := c.FetchStream(context.Background(), shopDS(), nil, &recHandler{})
	if !errors.Is(err, datasource.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

func TestListResourcesAndAncestors(t *testing.T) {
	c := newTestConnector(newFakeShop())
	ds := shopDS()

	schemas, err := c.ListResources(context.Background(), ds, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(schemas) != 2 || schemas[0].ExternalID != encodeSchemaID("shop") || !schemas[0].HasChildren {
		t.Fatalf("unexpected schemas: %+v", schemas)
	}

	tables, err := c.ListResources(context.Background(), ds, encodeSchemaID("shop"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != 2 || tables[0].ExternalID != encodeTableID("shop", "orders") || tables[0].ParentID != encodeSchemaID("shop") {
		t.Fatalf("unexpected tables: %+v", tables)
	}

	leaf, err := c.ListResources(context.Background(), ds, encodeTableID("shop", "orders"))
	if err != nil || leaf != nil {
		t.Fatalf("table listing should be empty, got %+v err=%v", leaf, err)
	}

	anc, err := c.ResolveResourceAncestors(context.Background(), ds, []string{
		encodeTableID("shop", "orders"), encodeTableID("shop", "users"), encodeSchemaID("analytics"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(anc) != 1 || anc[0] != encodeSchemaID("shop") {
		t.Fatalf("ancestors = %v", anc)
	}
}

func TestValidateReportsMissingDatabaseAsInvalidConfig(t *testing.T) {
	c := newTestConnector(newFakeShop())
	ds := shopDS()
	delete(ds.Settings, "database")
	if err := c.Validate(context.Background(), ds); !errors.Is(err, datasource.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

func TestConnectorTypesAndMetadata(t *testing.T) {
	if NewMySQLConnector().Type() != types.ConnectorTypeMySQL {
		t.Fatal("mysql connector type mismatch")
	}
	if NewPostgresConnector().Type() != types.ConnectorTypePostgres {
		t.Fatal("postgres connector type mismatch")
	}
}
