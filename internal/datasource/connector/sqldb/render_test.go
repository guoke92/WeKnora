package sqldb

import (
	"errors"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/datasource"
)

func TestResourceIDRoundTripAndSelection(t *testing.T) {
	schemaID := encodeSchemaID("shop")
	tableID := encodeTableID("shop", `we"ird.name`)

	p, err := parseResourceID(tableID)
	if err != nil || !p.IsTable || p.Schema != "shop" || p.Table != `we"ird.name` {
		t.Fatalf("table id round trip failed: %+v %v", p, err)
	}
	p, err = parseResourceID(schemaID)
	if err != nil || p.IsTable || p.Schema != "shop" {
		t.Fatalf("schema id round trip failed: %+v %v", p, err)
	}

	for _, bad := range []string{"", "shop", `[]`, `[""]`, `["a",""]`, `["a","b","c"]`} {
		if _, err := parseResourceID(bad); !errors.Is(err, datasource.ErrInvalidConfig) {
			t.Errorf("parseResourceID(%q) err = %v, want ErrInvalidConfig", bad, err)
		}
	}

	sel, err := parseSelection([]string{schemaID, encodeTableID("analytics", "events")})
	if err != nil {
		t.Fatal(err)
	}
	if !sel.covers("shop", "anything") {
		t.Error("schema selection must cover every table")
	}
	if !sel.covers("analytics", "events") || sel.covers("analytics", "other") {
		t.Error("table selection must cover only itself")
	}
	if got := strings.Join(sel.schemaNames(), ","); got != "analytics,shop" {
		t.Errorf("schemaNames = %s", got)
	}
}

func TestRenderIsDeterministicAndEscaped(t *testing.T) {
	cfg := baseConfig()
	meta := &tableMeta{
		Schema: "shop", Name: "orders", Comment: "订单 | 主表", RowEstimate: 1234567,
		Columns: []columnMeta{
			{Name: "id", DataType: "bigint", ColumnType: "bigint", Comment: "主键"},
			{Name: "remark", DataType: "varchar", ColumnType: "varchar(64)", Nullable: true, Comment: "a|b\nc"},
		},
		PrimaryKey: []string{"id"},
		ForeignKeys: []foreignKeyMeta{
			{Name: "fk_user", Columns: []string{"user_id"}, RefSchema: "shop", RefTable: "users", RefColumns: []string{"id"}},
		},
	}
	prof := &tableProfile{
		Sampled: 10, Mode: "latest by primary key",
		Columns: []colStat{
			{Name: "id", Role: roleID, Sampled: 10, NonNull: 10, HasRange: true, Min: "1", Max: "99", Distinct: 10},
			{Name: "remark", Role: roleText, Sampled: 10, NonNull: 4, Distinct: 2,
				Top: []valueShare{{Value: "x|y", Count: 3, Share: 0.75}, {Value: "", Count: 1, Share: 0.25}}},
		},
	}

	a := renderTable(cfg, meta, prof, "", nil, nil)
	b := renderTable(cfg, meta, prof, "", nil, nil)
	if a != b {
		t.Fatal("render is not deterministic")
	}
	if !strings.Contains(a, "估算行数：约 1200000") {
		t.Errorf("row estimate not bucketed:\n%s", a)
	}
	if !strings.Contains(a, `订单 | 主表`) {
		t.Errorf("table comment should be present verbatim")
	}
	if !strings.Contains(a, `a\|b c`) {
		t.Errorf("pipe and newline in cell not escaped:\n%s", a)
	}
	if !strings.Contains(a, `x\|y (75.0%)`) || !strings.Contains(a, "（空字符串） (25.0%)") {
		t.Errorf("TopK rendering wrong:\n%s", a)
	}
	if !strings.Contains(a, "`fk_user`: (user_id) → shop.users (id)") && !strings.Contains(a, "`fk_user`: (user_id) → users (id)") {
		t.Errorf("foreign key missing:\n%s", a)
	}
}

func TestRenderWithoutProfileAndWithNote(t *testing.T) {
	cfg := baseConfig()
	meta := &tableMeta{Schema: "shop", Name: "t", RowEstimate: -1,
		Columns: []columnMeta{{Name: "a", ColumnType: "int"}}}

	noProfile := renderTable(cfg, meta, nil, "", nil, nil)
	if strings.Contains(noProfile, "列画像") {
		t.Errorf("profile section must be omitted when profiling is off")
	}
	if !strings.Contains(noProfile, "未知") {
		t.Errorf("unknown row estimate should be explicit")
	}

	failed := renderTable(cfg, meta, nil, "采样失败", nil, nil)
	if !strings.Contains(failed, "列画像暂不可用：采样失败") {
		t.Errorf("failed profile should carry a note:\n%s", failed)
	}
}

func TestApproxRowsBuckets(t *testing.T) {
	cases := map[int64]string{-1: "未知（尚未收集统计信息）", 0: "0", 42: "42", 1234: "约 1200", 1250: "约 1300", 99999: "约 100000"}
	for in, want := range cases {
		if got := approxRows(in); got != want {
			t.Errorf("approxRows(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestQualityHints(t *testing.T) {
	prof := &tableProfile{Columns: []colStat{
		{Name: "mostly_null", Role: roleText, Sampled: 100, NonNull: 2},
		{Name: "constant", Role: roleText, Sampled: 100, NonNull: 100, Distinct: 1},
		{Name: "blobby", Role: roleLarge, Sampled: 100, NonNull: 1, Distinct: -1},
	}}
	// mostly_null and blobby are both mostly empty; constant has a single value.
	hints := qualityHints(prof)
	if len(hints) != 3 {
		t.Fatalf("hints = %v", hints)
	}
}
