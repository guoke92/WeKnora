package sqldb

import (
	"strconv"
	"strings"
	"testing"
)

// keyFrom builds the key sketch of a numeric column from its sampled values.
func keyFrom(name string, values []string) keySketch {
	return sketchOfValues(name, values)
}

// countsOf counts how often each value occurs.
func countsOf(values []string) map[string]int {
	counts := make(map[string]int, len(values))
	for _, v := range values {
		counts[v]++
	}
	return counts
}

// repeatEach returns every value n times in order, so each value appears in n rows.
func repeatEach(values []string, n int) []string {
	out := make([]string, 0, len(values)*n)
	for _, v := range values {
		for i := 0; i < n; i++ {
			out = append(out, v)
		}
	}
	return out
}

func findRelation(findings []relationFinding, kind, fromTable, fromCol, toTable, toCol string) *relationFinding {
	for i := range findings {
		f := &findings[i]
		if f.Kind == kind && f.From.Table == fromTable && f.From.Column == fromCol &&
			f.To.Table == toTable && f.To.Column == toCol {
			return f
		}
	}
	return nil
}

func TestDiscoverFindsUndeclaredForeignKey(t *testing.T) {
	users := relTable{
		ID: encodeTableID("shop", "users"), Schema: "shop", Name: "users",
		Meta: &tableMeta{Schema: "shop", Name: "users"},
		Keys: []keySketch{keyFrom("id", seqRange(1, 1000))},
	}
	// buyer_id takes 300 of the 1000 user ids, every value repeated, so it is not unique.
	var buyer []string
	for i := 0; i < 1000; i++ {
		buyer = append(buyer, strconv.Itoa(i*7%300+1))
	}
	orders := relTable{
		ID: encodeTableID("shop", "orders"), Schema: "shop", Name: "orders",
		Meta: &tableMeta{Schema: "shop", Name: "orders"},
		Keys: []keySketch{
			keyFrom("buyer_id", buyer),
			keyFrom("id", seqRange(5001, 6000)),
			keyFrom("status", repeatEach(seqRange(1, 5), 200)), // too few distinct values to judge
		},
	}
	profiles := relTable{
		ID: encodeTableID("shop", "profiles"), Schema: "shop", Name: "profiles",
		Meta: &tableMeta{
			Schema: "shop", Name: "profiles",
			ForeignKeys: []foreignKeyMeta{{
				Name: "fk_user", Columns: []string{"user_id"}, RefSchema: "shop", RefTable: "users", RefColumns: []string{"id"},
			}},
		},
		Keys: []keySketch{keyFrom("user_id", seqRange(1, 1000))},
	}

	findings, skipped := discoverRelations([]relTable{users, orders, profiles})
	if skipped {
		t.Fatal("selection should not be skipped")
	}
	f := findRelation(findings, relKindFK, "orders", "buyer_id", "users", "id")
	if f == nil {
		t.Fatalf("buyer_id -> users.id not inferred; findings = %+v", findings)
	}
	if f.Coverage < 0.9 || f.ToDistinct != 1000 || f.FromDistinct != 300 {
		t.Fatalf("evidence = %+v", f)
	}
	if findRelation(findings, relKindFK, "profiles", "user_id", "users", "id") != nil ||
		findRelation(findings, relKindOverlap, "profiles", "user_id", "users", "id") != nil {
		t.Fatalf("declared foreign key reported again: %+v", findings)
	}
	for _, x := range findings {
		if x.From.Column == "status" || x.To.Column == "status" {
			t.Fatalf("low-cardinality column should not take part: %+v", x)
		}
		if x.From.Column == "id" && x.To.Column == "id" {
			t.Fatalf("disjoint id ranges should not relate: %+v", x)
		}
	}
}

func TestDiscoverReportsOverlapWithoutKeyDirection(t *testing.T) {
	a := relTable{
		ID: encodeTableID("shop", "a"), Schema: "shop", Name: "a",
		Keys: []keySketch{keyFrom("code", repeatEach(seqRange(1, 100), 3))},
	}
	b := relTable{
		ID: encodeTableID("shop", "b"), Schema: "shop", Name: "b",
		Keys: []keySketch{keyFrom("code", repeatEach(seqRange(21, 120), 3))},
	}
	findings, _ := discoverRelations([]relTable{a, b})
	if len(findings) != 1 || findings[0].Kind != relKindOverlap {
		t.Fatalf("findings = %+v, want one overlap", findings)
	}
	if findings[0].Jaccard < 0.6 || findings[0].Jaccard > 0.75 {
		t.Fatalf("jaccard = %v, want about 0.67", findings[0].Jaccard)
	}
}

func TestDiscoverKeepsNumericAndTextApart(t *testing.T) {
	var words []string
	for i := 1; i <= 1000; i++ {
		words = append(words, "w"+strconv.Itoa(i))
	}
	numbers := keyFrom("id", seqRange(1, 1000))
	text := buildKeySketch("name", false, 1000, countsOf(words))
	a := relTable{ID: "a", Schema: "s", Name: "a", Keys: []keySketch{numbers}}
	b := relTable{ID: "b", Schema: "s", Name: "b", Keys: []keySketch{text}}
	if findings, _ := discoverRelations([]relTable{a, b}); len(findings) != 0 {
		t.Fatalf("numeric and text columns should not be compared: %+v", findings)
	}
}

func TestDiscoverSkipsOversizedSelection(t *testing.T) {
	var keys []keySketch
	for i := 0; i <= relMaxKeyColumns; i++ {
		keys = append(keys, keyFrom("c"+strconv.Itoa(i), seqRange(1, 20)))
	}
	findings, skipped := discoverRelations([]relTable{{ID: "x", Schema: "s", Name: "t", Keys: keys}})
	if !skipped || len(findings) != 0 {
		t.Fatalf("skipped=%v findings=%d, want skipped with none", skipped, len(findings))
	}
}

func TestRelationsRenderBothEnds(t *testing.T) {
	findings := []relationFinding{{
		Kind:         relKindFK,
		From:         relationEnd{TableID: "o", Schema: "shop", Table: "orders", Column: "buyer_id"},
		To:           relationEnd{TableID: "u", Schema: "shop", Table: "users", Column: "id"},
		Coverage:     0.97,
		FromDistinct: 300,
		ToDistinct:   1000,
	}}
	rels := relationsByTable(findings, []string{"o", "u", "x"}, false)

	var out strings.Builder
	writeRelations(&out, rels["o"])
	for _, want := range []string{"`buyer_id` → `shop.users.id`", "约 97%", "推断关联"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("orders section missing %q:\n%s", want, out.String())
		}
	}

	var in strings.Builder
	writeRelations(&in, rels["u"])
	if !strings.Contains(in.String(), "`id` ← `shop.orders.buyer_id`") || !strings.Contains(in.String(), "推断关联") {
		t.Fatalf("users section missing incoming relation:\n%s", in.String())
	}

	var none strings.Builder
	writeRelations(&none, rels["x"])
	if !strings.Contains(none.String(), "未发现推断关系") {
		t.Fatalf("empty section should say none found:\n%s", none.String())
	}

	var skipped strings.Builder
	writeRelations(&skipped, relationsByTable(nil, []string{"s"}, true)["s"])
	if !strings.Contains(skipped.String(), "已跳过") {
		t.Fatalf("skipped section should say so:\n%s", skipped.String())
	}
}

// newFakeRelationalShop has an undeclared buyer_id -> users.id and a declared
// profiles.user_id -> users.id, both backed by sample rows.
func newFakeRelationalShop() *fakeDialect {
	users := &tableMeta{
		Schema: "shop", Name: "users", RowEstimate: 1000,
		Columns: []columnMeta{
			{Name: "id", DataType: "bigint", ColumnType: "bigint"},
			{Name: "name", DataType: "varchar", ColumnType: "varchar(64)"},
		},
		PrimaryKey: []string{"id"},
	}
	orders := &tableMeta{
		Schema: "shop", Name: "orders", RowEstimate: 1000,
		Columns: []columnMeta{
			{Name: "id", DataType: "bigint", ColumnType: "bigint"},
			{Name: "buyer_id", DataType: "bigint", ColumnType: "bigint"},
			{Name: "status", DataType: "varchar", ColumnType: "varchar(16)"},
		},
		PrimaryKey: []string{"id"},
	}
	profiles := &tableMeta{
		Schema: "shop", Name: "profiles", RowEstimate: 1000,
		Columns: []columnMeta{
			{Name: "id", DataType: "bigint", ColumnType: "bigint"},
			{Name: "user_id", DataType: "bigint", ColumnType: "bigint"},
		},
		PrimaryKey: []string{"id"},
		ForeignKeys: []foreignKeyMeta{{
			Name: "fk_user", Columns: []string{"user_id"}, RefSchema: "shop", RefTable: "users", RefColumns: []string{"id"},
		}},
	}

	var userRows, orderRows, profileRows []map[string]interface{}
	for i := 1; i <= 1000; i++ {
		userRows = append(userRows, map[string]interface{}{"id": int64(i), "name": "u" + strconv.Itoa(i)})
		profileRows = append(profileRows, map[string]interface{}{"id": int64(i), "user_id": int64(i)})
		orderRows = append(orderRows, map[string]interface{}{
			"id":       int64(5000 + i),
			"buyer_id": int64((i*7)%300 + 1),
			"status":   []string{"new", "paid", "done", "void", "hold"}[i%5],
		})
	}
	return &fakeDialect{
		schemas: []string{"shop"},
		tables: map[string][]tableRef{"shop": {
			{Name: "orders", RowEstimate: 1000}, {Name: "profiles", RowEstimate: 1000}, {Name: "users", RowEstimate: 1000},
		}},
		metas: map[string]*tableMeta{
			"shop.users": users, "shop.orders": orders, "shop.profiles": profiles,
		},
		samples: map[string][]map[string]interface{}{
			"shop.users": userRows, "shop.orders": orderRows, "shop.profiles": profileRows,
		},
	}
}

func docOf(t *testing.T, h *recHandler, table string) string {
	t.Helper()
	for _, it := range h.items {
		if it.ExternalID == encodeTableID("shop", table) {
			return string(it.Content)
		}
	}
	t.Fatalf("no document emitted for %s", table)
	return ""
}

func TestSyncRendersInferredRelationsInBothDocuments(t *testing.T) {
	d := newFakeRelationalShop()
	c := newTestConnector(d)
	ds := shopDS(encodeSchemaID("shop"))
	h, cur := runOnce(t, c, ds, nil, false)

	orders := docOf(t, h, "orders")
	if !strings.Contains(orders, "## 推断关系") || !strings.Contains(orders, "`buyer_id` → `shop.users.id`") {
		t.Fatalf("orders document lacks the inferred foreign key:\n%s", orders)
	}
	users := docOf(t, h, "users")
	if !strings.Contains(users, "`id` ← `shop.orders.buyer_id`") {
		t.Fatalf("users document lacks the incoming relation:\n%s", users)
	}
	profiles := docOf(t, h, "profiles")
	if strings.Contains(profiles, "`user_id` → `shop.users.id`") {
		t.Fatalf("declared foreign key repeated as inferred:\n%s", profiles)
	}

	// Discovery is deterministic: an unchanged database re-emits nothing.
	again, _ := runOnce(t, c, ds, cur, false)
	if got := again.emittedIDs(); len(got) != 0 {
		t.Fatalf("unchanged run re-emitted %v", got)
	}
}

func TestSyncWithoutProfileOmitsRelations(t *testing.T) {
	c := newTestConnector(newFakeRelationalShop())
	ds := shopDS(encodeSchemaID("shop"))
	ds.Settings["profile_enabled"] = false
	h, _ := runOnce(t, c, ds, nil, false)
	for _, it := range h.items {
		if strings.Contains(string(it.Content), "推断关系") {
			t.Fatalf("relations rendered without profiling: %s", it.ExternalID)
		}
	}
}

// Relations must not change the rendered output for a declared-only schema: a table
// that has no inferred relation still gets an explicit "none found" section.
func TestNoRelationsStillRendersSection(t *testing.T) {
	c := newTestConnector(newFakeShop())
	h, _ := runOnce(t, c, shopDS(encodeSchemaID("shop")), nil, false)
	if got := docOf(t, h, "users"); !strings.Contains(got, "未发现推断关系") {
		t.Fatalf("users document should state that no relations were found:\n%s", got)
	}
}
