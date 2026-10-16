package sqldb

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/types"
)

func baseConfig() config {
	return config{
		engine:         engineMySQL,
		host:           "db.internal",
		port:           3306,
		database:       "shop",
		profileEnabled: true,
		profileValues:  true,
		topK:           10,
		sampleRows:     1000,
		maxTables:      200,
		queryTimeout:   defaultQueryTimeout,
		maskedColumns:  map[string]bool{},
	}
}

func TestNormalizeType(t *testing.T) {
	cases := map[string]string{
		"varchar(64)":                    "varchar",
		"int(11) unsigned":               "int",
		"numeric(12,2)":                  "numeric",
		"character varying(20)":          "character varying",
		"timestamp(6) without time zone": "timestamp without time zone",
		"DOUBLE PRECISION":               "double precision",
	}
	for in, want := range cases {
		if got := normalizeType(in); got != want {
			t.Errorf("normalizeType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBaseRoleClassification(t *testing.T) {
	cases := map[string]colRole{
		"bigint":                   roleNumeric,
		"decimal(10,2)":            roleNumeric,
		"varchar(32)":              roleText,
		"enum":                     roleText,
		"datetime":                 roleTime,
		"timestamp with time zone": roleTime,
		"tinyint(1)":               roleNumeric,
		"bool":                     roleBool,
		"text":                     roleLarge,
		"longblob":                 roleLarge,
		"jsonb":                    roleLarge,
		"bit":                      roleOther,
		"integer[]":                roleOther,
	}
	for dt, want := range cases {
		if got := baseRole(dt); got != want {
			t.Errorf("baseRole(%q) = %v, want %v", dt, got, want)
		}
	}
}

func TestPlanColumnsRolesAndSensitivity(t *testing.T) {
	meta := &tableMeta{
		Schema: "shop", Name: "users",
		Columns: []columnMeta{
			{Name: "id", DataType: "bigint"},
			{Name: "owner_id", DataType: "int"},
			{Name: "email", DataType: "varchar"},
			{Name: "nickname", DataType: "varchar"},
			{Name: "card_no", DataType: "varchar"},
			{Name: "note", DataType: "varchar"},
		},
		PrimaryKey: []string{"id"},
		Indexes:    []indexMeta{{Name: "uk_note", Unique: true, Columns: []string{"note"}}},
	}
	cfg := baseConfig()
	cfg.maskedColumns = map[string]bool{"nickname": true}
	plans := planColumns(meta, cfg)

	want := []colRole{roleID, roleID, roleMasked, roleMasked, roleMasked, roleID}
	for i, p := range plans {
		if p.role != want[i] {
			t.Errorf("column %s role = %v, want %v", meta.Columns[i].Name, p.role, want[i])
		}
	}
}

func makeRow(plans []colPlan, meta *tableMeta, values map[string]interface{}, width int) []interface{} {
	row := make([]interface{}, width)
	for _, p := range plans {
		name := meta.Columns[p.idx].Name
		v := values[name]
		if p.valIdx >= 0 {
			row[p.valIdx] = v
		}
		if p.nullIdx >= 0 {
			row[p.nullIdx] = v == nil
		}
		if p.lenIdx >= 0 {
			if s, ok := v.(string); ok {
				row[p.lenIdx] = int64(len(s))
			} else {
				row[p.lenIdx] = int64(-1)
			}
		}
	}
	return row
}

func findStat(t *testing.T, prof *tableProfile, name string) colStat {
	t.Helper()
	for _, s := range prof.Columns {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no stat for %s", name)
	return colStat{}
}

func sampleMeta() *tableMeta {
	return &tableMeta{
		Schema: "shop", Name: "orders",
		Columns: []columnMeta{
			{Name: "id", DataType: "bigint"},
			{Name: "status", DataType: "varchar"},
			{Name: "amount", DataType: "decimal"},
			{Name: "created_at", DataType: "datetime"},
			{Name: "body", DataType: "text"},
			{Name: "buyer_email", DataType: "varchar"},
			{Name: "note", DataType: "varchar"},
		},
		PrimaryKey: []string{"id"},
	}
}

func TestComputeProfileFeatures(t *testing.T) {
	meta := sampleMeta()
	cfg := baseConfig()
	plans := planColumns(meta, cfg)
	spec := buildSpec(&mysqlDialect{}, meta, plans, cfg.sampleRows)
	if spec == nil {
		t.Fatal("expected a sample spec")
	}
	if spec.timeIdx < 0 {
		t.Fatal("expected a time column for latest-value tracking")
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := [][]interface{}{
		makeRow(plans, meta, map[string]interface{}{
			"id": "1", "status": "paid", "amount": "10.50", "created_at": base,
			"body": "a long body", "buyer_email": "x@y.z", "note": "n1",
		}, spec.width),
		makeRow(plans, meta, map[string]interface{}{
			"id": "5", "status": "paid", "amount": "20.00", "created_at": base.Add(48 * time.Hour),
			"body": nil, "buyer_email": "p@q.r", "note": "n2",
		}, spec.width),
		makeRow(plans, meta, map[string]interface{}{
			"id": "3", "status": "refunded", "amount": "30.00", "created_at": base.Add(24 * time.Hour),
			"body": "another", "buyer_email": nil, "note": nil,
		}, spec.width),
	}

	prof := computeProfile(meta, spec, rows, cfg)
	if prof.Sampled != 3 {
		t.Fatalf("sampled = %d", prof.Sampled)
	}

	id := findStat(t, prof, "id")
	if id.Role != roleID || !id.HasRange || id.Min != "1" || id.Max != "5" {
		t.Errorf("id stat = %+v", id)
	}

	// Three rows with two distinct values do not repeat enough for TopK (see TestTopKRequiresRepeatingValues).
	status := findStat(t, prof, "status")
	if len(status.Top) != 0 || status.Distinct != 2 {
		t.Errorf("status = %+v", status)
	}

	amount := findStat(t, prof, "amount")
	if !amount.HasAvg || amount.Min != "10.50" || amount.Max != "30.00" {
		t.Errorf("amount stat = %+v", amount)
	}

	created := findStat(t, prof, "created_at")
	if !created.HasRange || created.Min != "2026-01-01 00:00:00" || created.Max != "2026-01-03 00:00:00" {
		t.Errorf("created_at range = %q ~ %q", created.Min, created.Max)
	}

	body := findStat(t, prof, "body")
	if body.Role != roleLarge || body.NonNull != 2 {
		t.Errorf("body non-null = %d role=%v", body.NonNull, body.Role)
	}
	if body.Latest != "2026-01-02 00:00:00" {
		t.Errorf("body latest non-null time = %q, want the row with created_at 2026-01-02 (its body is non-null)", body.Latest)
	}
	if len(body.Top) != 0 || body.HasRange {
		t.Errorf("large columns must not expose values")
	}

	email := findStat(t, prof, "buyer_email")
	if email.Role != roleMasked || !email.Hidden || len(email.Top) != 0 || email.HasRange {
		t.Errorf("sensitive column leaked values: %+v", email)
	}
	if email.NonNull != 2 {
		t.Errorf("sensitive non-null = %d, want 2", email.NonNull)
	}

	note := findStat(t, prof, "note")
	if note.Distinct != 2 || note.NonNull != 2 {
		t.Errorf("note distinct = %d non-null = %d", note.Distinct, note.NonNull)
	}
}

func TestProfileValuesOffHidesRangesAndTopK(t *testing.T) {
	meta := sampleMeta()
	cfg := baseConfig()
	cfg.profileValues = false
	plans := planColumns(meta, cfg)
	spec := buildSpec(&mysqlDialect{}, meta, plans, cfg.sampleRows)
	rows := [][]interface{}{makeRow(plans, meta, map[string]interface{}{"id": "1", "status": "paid"}, spec.width)}

	prof := computeProfile(meta, spec, rows, cfg)
	status := findStat(t, prof, "status")
	if len(status.Top) != 0 || !status.Hidden {
		t.Errorf("values must be hidden when profile_values is off: %+v", status)
	}
	id := findStat(t, prof, "id")
	if id.HasRange {
		t.Errorf("ranges must be hidden when profile_values is off")
	}
}

func TestMaskedColumnOverrideByConfig(t *testing.T) {
	meta := sampleMeta()
	cfg := baseConfig()
	cfg.maskedColumns = map[string]bool{"status": true}
	plans := planColumns(meta, cfg)
	for _, p := range plans {
		if meta.Columns[p.idx].Name == "status" && p.role != roleMasked {
			t.Fatalf("configured masked column not masked: role=%v", p.role)
		}
	}
}

func TestTopKTieBreakIsDeterministic(t *testing.T) {
	counts := map[string]int{"b": 2, "a": 2, "c": 5}
	got := topK(counts, 9, 2)
	if len(got) != 2 || got[0].Value != "c" || got[1].Value != "a" {
		t.Fatalf("topK = %+v", got)
	}
}

func TestParseConfigDefaultsAndValidation(t *testing.T) {
	ds := &types.DataSourceConfig{
		Credentials: map[string]interface{}{"user": " reader ", "password": "p@ss word"},
		Settings: map[string]interface{}{
			"host":                  "db.internal",
			"database":              "shop",
			"topk":                  float64(500),
			"profile_sample_rows":   "250",
			"masked_columns":        "ssn_hash, Remark\nnote2",
			"profile_values":        false,
			"query_timeout_seconds": float64(0),
		},
	}
	cfg, err := parseConfig(engineMySQL, ds)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.port != 3306 || cfg.tls != tlsRequire || cfg.topK != maxTopK || cfg.sampleRows != 250 {
		t.Fatalf("defaults/clamps wrong: %+v", cfg)
	}
	if cfg.user != "reader" || cfg.password != "p@ss word" {
		t.Fatalf("credentials not normalised: user=%q password=%q", cfg.user, cfg.password)
	}
	if !cfg.maskedColumns["ssn_hash"] || !cfg.maskedColumns["remark"] || !cfg.maskedColumns["note2"] {
		t.Fatalf("masked columns = %v", cfg.maskedColumns)
	}
	if cfg.profileValues || !cfg.profileEnabled {
		t.Fatalf("profile flags wrong: values=%v enabled=%v", cfg.profileValues, cfg.profileEnabled)
	}
	if cfg.queryTimeout != time.Second {
		t.Fatalf("query timeout = %v, want clamp to 1s", cfg.queryTimeout)
	}

	pg, err := parseConfig(enginePostgres, ds)
	if err != nil || pg.port != 5432 {
		t.Fatalf("postgres default port: %v %d", err, pg.port)
	}

	bad := &types.DataSourceConfig{
		Credentials: map[string]interface{}{"user": "u"},
		Settings:    map[string]interface{}{"host": "h", "database": "d", "tls_mode": "plain"},
	}
	if _, err := parseConfig(engineMySQL, bad); !strings.Contains(err.Error(), "tls_mode") {
		t.Fatalf("invalid tls_mode accepted: %v", err)
	}
	noUser := &types.DataSourceConfig{Settings: map[string]interface{}{"host": "h", "database": "d"}}
	if _, err := parseConfig(engineMySQL, noUser); err == nil {
		t.Fatal("missing user accepted")
	}
	if _, err := parseConfig(engineMySQL, nil); err == nil {
		t.Fatal("nil config accepted")
	}
	if _, err := parseConfig(engineMySQL, noUser); !isInvalidConfig(err) {
		t.Fatalf("error should be ErrInvalidConfig: %v", err)
	}
}

func isInvalidConfig(err error) bool {
	return err != nil && strings.Contains(err.Error(), datasource.ErrInvalidConfig.Error())
}

func TestPGDSNQuotesValues(t *testing.T) {
	cfg := baseConfig()
	cfg.engine = enginePostgres
	cfg.user = "reader"
	cfg.password = `it's\pw`
	cfg.tls = tlsVerifyFull
	dsn := pgDSN(cfg)
	if !strings.Contains(dsn, `password='it\'s\\pw'`) {
		t.Fatalf("password not escaped: %s", dsn)
	}
	if !strings.Contains(dsn, "sslmode=verify-full") {
		t.Fatalf("sslmode missing: %s", dsn)
	}
}

func TestUnsignedIntegersAreReadAsNumbers(t *testing.T) {
	if got := asInt64(uint64(1234567)); got != 1234567 {
		t.Fatalf("asInt64(uint64) = %d", got)
	}
	if got := asInt64(uint64(1) << 63); got != -1 {
		t.Fatalf("asInt64 must not overflow, got %d", got)
	}
	if f, ok := toFloat(uint64(42)); !ok || f != 42 {
		t.Fatalf("toFloat(uint64) = %v %v", f, ok)
	}
}

func TestTopKRequiresRepeatingValues(t *testing.T) {
	meta := sampleMeta()
	cfg := baseConfig()
	plans := planColumns(meta, cfg)
	spec := buildSpec(&mysqlDialect{}, meta, plans, cfg.sampleRows)

	var rows [][]interface{}
	for i := 0; i < 6; i++ {
		status := "paid"
		if i >= 4 {
			status = "refunded"
		}
		rows = append(rows, makeRow(plans, meta, map[string]interface{}{"id": "1", "status": status}, spec.width))
	}
	prof := computeProfile(meta, spec, rows, cfg)
	status := findStat(t, prof, "status")
	if len(status.Top) != 2 || status.Top[0].Value != "paid" || status.Top[0].Count != 4 {
		t.Fatalf("status TopK = %+v", status.Top)
	}
	if got := status.Top[0].Share; got < 0.66 || got > 0.67 {
		t.Errorf("status top share = %v", got)
	}

	// Every value unique: the distinct count is reported and no TopK list is produced.
	var unique [][]interface{}
	for i := 0; i < 6; i++ {
		unique = append(unique, makeRow(plans, meta, map[string]interface{}{"id": "1", "note": "n" + strconv.Itoa(i)}, spec.width))
	}
	prof = computeProfile(meta, spec, unique, cfg)
	note := findStat(t, prof, "note")
	if len(note.Top) != 0 || note.Distinct != 6 {
		t.Errorf("unique column should not list TopK: %+v", note)
	}
}

func TestIDAverageIsNumericNotLength(t *testing.T) {
	meta := sampleMeta()
	cfg := baseConfig()
	plans := planColumns(meta, cfg)
	spec := buildSpec(&mysqlDialect{}, meta, plans, cfg.sampleRows)
	rows := [][]interface{}{
		makeRow(plans, meta, map[string]interface{}{"id": int64(1000)}, spec.width),
		makeRow(plans, meta, map[string]interface{}{"id": int64(3000)}, spec.width),
	}
	id := findStat(t, computeProfile(meta, spec, rows, cfg), "id")
	if id.HasAvg {
		t.Fatalf("numeric id should not report an average: %+v", id)
	}
}
