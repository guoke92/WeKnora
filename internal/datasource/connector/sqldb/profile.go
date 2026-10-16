package sqldb

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Column profiling (L2). Features are computed per column from one bounded sample of
// rows, never per row: identifiers and times yield ranges, low-cardinality values yield
// TopK shares, large text/blob/json columns yield non-null ratio, average length and the
// latest non-null time, and sensitive columns yield only the non-null ratio.

// colRole is the profiling policy chosen for a column.
type colRole int

const (
	roleOther   colRole = iota // unknown type: non-null ratio only
	roleID                     // identifier: range (numeric/time) or low-cardinality TopK
	roleTime                   // date/time: range
	roleNumeric                // number: range, average, TopK when low-cardinality
	roleBool                   // boolean: TopK
	roleText                   // text/varchar/enum: TopK when low-cardinality, otherwise distinct count
	roleLarge                  // text/blob/json long values: non-null ratio, average length, latest time
	roleMasked                 // sensitive: non-null ratio only, never values
)

const (
	textPrefixLen        = 256 // characters/bytes read per text value
	categoricalMaxCard   = 500 // distinct values allowed before TopK is suppressed
	idTopKMaxCard        = 50  // identifiers only show TopK when very low-cardinality
	topKDisplayRuneLimit = 60
	timeLayout           = "2006-01-02 15:04:05"
)

var (
	numericTypes = setOf("tinyint", "smallint", "mediumint", "int", "integer", "bigint", "int2", "int4", "int8",
		"decimal", "numeric", "float", "float4", "float8", "double", "double precision", "real", "year", "money")
	timeTypes = setOf("date", "datetime", "timestamp", "timestamptz", "time",
		"timestamp without time zone", "timestamp with time zone", "time without time zone", "time with time zone")
	boolTypes  = setOf("bool", "boolean")
	textTypes  = setOf("char", "varchar", "character", "character varying", "bpchar", "enum", "set", "uuid", "citext", "name")
	largeTypes = setOf("text", "tinytext", "mediumtext", "longtext", "blob", "tinyblob", "mediumblob", "longblob",
		"json", "jsonb", "bytea", "xml", "binary", "varbinary", "geometry", "point", "linestring", "polygon",
		"multipoint", "multilinestring", "multipolygon", "geometrycollection")
)

func setOf(values ...string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, v := range values {
		m[v] = true
	}
	return m
}

// sensitiveNameFragments mark columns whose values are never shown (sensitive policy).
var sensitiveNameFragments = []string{
	"password", "passwd", "pwd", "secret", "token", "phone", "mobile",
	"id_card", "idcard", "ssn", "bank", "card_no", "email",
}

func isSensitiveName(name string, configured map[string]bool) bool {
	lower := strings.ToLower(name)
	if configured[lower] {
		return true
	}
	for _, f := range sensitiveNameFragments {
		if strings.Contains(lower, f) {
			return true
		}
	}
	return false
}

// normalizeType lower-cases a type name and removes parameters and modifiers:
// "varchar(64)" -> "varchar", "int(11) unsigned" -> "int", "numeric(12,2)" -> "numeric".
func normalizeType(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for {
		open := strings.IndexByte(s, '(')
		if open < 0 {
			break
		}
		closeIdx := strings.IndexByte(s[open:], ')')
		if closeIdx < 0 {
			s = s[:open]
			break
		}
		s = s[:open] + s[open+closeIdx+1:]
	}
	s = strings.ReplaceAll(s, " unsigned", "")
	s = strings.ReplaceAll(s, " zerofill", "")
	return strings.Join(strings.Fields(s), " ")
}

// baseRole maps an engine data type to the category it profiles as, before sensitivity
// and identifier rules are applied.
func baseRole(dataType string) colRole {
	t := normalizeType(dataType)
	switch {
	case largeTypes[t]:
		return roleLarge
	case textTypes[t]:
		return roleText
	case numericTypes[t]:
		return roleNumeric
	case timeTypes[t]:
		return roleTime
	case boolTypes[t]:
		return roleBool
	default:
		return roleOther
	}
}

// colPlan is one profiled column and the result-set slots its expressions occupy.
type colPlan struct {
	idx     int // index into meta.Columns
	role    colRole
	base    colRole // category from the data type alone
	valIdx  int     // value slot, -1 when absent
	nullIdx int     // null-flag slot, -1 when absent
	lenIdx  int     // length slot, -1 when absent
}

// planColumns decides the profiling role of every column (up to maxProfileColumns).
func planColumns(meta *tableMeta, cfg config) []colPlan {
	uniqueSingle := map[string]bool{}
	if len(meta.PrimaryKey) == 1 {
		uniqueSingle[meta.PrimaryKey[0]] = true
	}
	for _, idx := range meta.Indexes {
		if idx.Unique && len(idx.Columns) == 1 {
			uniqueSingle[idx.Columns[0]] = true
		}
	}

	limit := len(meta.Columns)
	if limit > maxProfileColumns {
		limit = maxProfileColumns
	}
	plans := make([]colPlan, 0, limit)
	for i := 0; i < limit; i++ {
		col := meta.Columns[i]
		base := baseRole(col.DataType)
		role := base
		lower := strings.ToLower(col.Name)
		switch {
		case isSensitiveName(col.Name, cfg.maskedColumns):
			role = roleMasked
		case (base == roleNumeric || base == roleText) &&
			(lower == "id" || strings.HasSuffix(lower, "_id") || uniqueSingle[col.Name]):
			role = roleID
		}
		plans = append(plans, colPlan{idx: i, role: role, base: base, valIdx: -1, nullIdx: -1, lenIdx: -1})
	}
	return plans
}

// sampleSpec is the sample query for one table and how to read its rows.
type sampleSpec struct {
	query   string
	width   int
	mode    string
	plans   []colPlan
	timeIdx int // index into plans of the first time column, -1 if none
}

// buildSpec assembles one SELECT returning, per profiled column, only the aggregates
// needed for its role. Raw large values are never selected.
func buildSpec(d dialect, meta *tableMeta, plans []colPlan, limit int) *sampleSpec {
	var exprs []string
	add := func(e string) int {
		exprs = append(exprs, e)
		return len(exprs) - 1
	}
	spec := &sampleSpec{plans: plans, timeIdx: -1}

	for i := range plans {
		p := &plans[i]
		col := d.quoteIdent(meta.Columns[p.idx].Name)
		switch p.role {
		case roleMasked, roleOther:
			p.nullIdx = add(col + " IS NULL")
		case roleLarge:
			p.nullIdx = add(col + " IS NULL")
			p.lenIdx = add(d.lengthOf(col))
		case roleID, roleText:
			if p.base == roleNumeric {
				p.valIdx = add(col)
			} else {
				p.valIdx = add(fmt.Sprintf("LEFT(%s, %d)", d.textOf(col), textPrefixLen))
			}
		default: // numeric, time, bool
			p.valIdx = add(col)
		}
		if p.role == roleTime && spec.timeIdx < 0 {
			spec.timeIdx = i
		}
	}
	if len(exprs) == 0 {
		return nil
	}

	fromExtra, tail, mode := d.sampleClause(meta, limit)
	spec.mode = mode
	spec.width = len(exprs)
	spec.query = fmt.Sprintf("SELECT %s FROM %s.%s%s %s",
		strings.Join(exprs, ", "), d.quoteIdent(meta.Schema), d.quoteIdent(meta.Name), fromExtra, tail)
	return spec
}

// valueKind describes how a scalar slot must be ordered.
type valueKind int

const (
	kindLexical valueKind = iota
	kindNumeric
	kindTime
)

func (p colPlan) valueKind() valueKind {
	switch p.base {
	case roleNumeric:
		return kindNumeric
	case roleTime:
		return kindTime
	default:
		return kindLexical
	}
}

// valueShare is one entry of a TopK list.
type valueShare struct {
	Value string
	Count int
	Share float64 // fraction of non-null sampled rows
}

// colStat is the computed profile of one column.
type colStat struct {
	Name        string
	Type        string
	Role        colRole
	Sampled     int
	NonNull     int
	Distinct    int // exact within the sample; -1 when not computed
	HasRange    bool
	Min, Max    string
	HasAvg      bool
	Avg         float64 // numeric average, or average text length for text/large
	AvgIsLength bool
	Top         []valueShare
	Latest      string // latest non-null time (large columns with a time column)
	Hidden      bool   // values intentionally omitted (profile_values off or sensitive)
}

// tableProfile is the column-level profile of one table sample.
type tableProfile struct {
	Sampled int
	Mode    string
	Columns []colStat
	Skipped int // columns beyond maxProfileColumns

	// keys summarises the key-like columns for relation discovery (L3). It is not rendered.
	keys []keySketch
}

// profileTable reads one bounded sample and aggregates it per column.
func profileTable(ctx context.Context, d dialect, meta *tableMeta, cfg config) (*tableProfile, error) {
	plans := planColumns(meta, cfg)
	spec := buildSpec(d, meta, plans, cfg.sampleRows)
	if spec == nil {
		return &tableProfile{Mode: "none", Skipped: len(meta.Columns)}, nil
	}
	rows, err := d.queryRows(ctx, spec.query, spec.width)
	if err != nil {
		return nil, err
	}
	return computeProfile(meta, spec, rows, cfg), nil
}

// computeProfile aggregates sampled rows. It is pure and deterministic for a given
// input, which keeps rendered documents byte-stable across unchanged syncs.
func computeProfile(meta *tableMeta, spec *sampleSpec, rows [][]interface{}, cfg config) *tableProfile {
	prof := &tableProfile{Sampled: len(rows), Mode: spec.mode, Skipped: len(meta.Columns) - len(spec.plans)}

	var timePlan *colPlan
	if spec.timeIdx >= 0 {
		timePlan = &spec.plans[spec.timeIdx]
	}

	for i := range spec.plans {
		p := spec.plans[i]
		col := meta.Columns[p.idx]
		stat := colStat{
			Name:     col.Name,
			Type:     col.ColumnType,
			Role:     p.role,
			Sampled:  len(rows),
			Distinct: -1,
		}

		if p.valIdx < 0 {
			// Null-flag only (sensitive, unknown type, large).
			var lenSum float64
			var lenN int
			var latest minMax
			latest.kind = kindTime
			for _, row := range rows {
				if truthy(row[p.nullIdx]) {
					continue
				}
				stat.NonNull++
				if p.lenIdx >= 0 {
					if n := asInt64(row[p.lenIdx]); n >= 0 {
						lenSum += float64(n)
						lenN++
					}
				}
				if p.role == roleLarge && timePlan != nil && timePlan.valIdx >= 0 {
					if v := row[timePlan.valIdx]; v != nil {
						latest.add(v, asString(v))
					}
				}
			}
			if lenN > 0 {
				stat.HasAvg = true
				stat.AvgIsLength = true
				stat.Avg = lenSum / float64(lenN)
			}
			if p.role == roleLarge && latest.set {
				stat.Latest = latest.maxRaw
			}
			if p.role == roleMasked {
				stat.Hidden = true
			}
			prof.Columns = append(prof.Columns, stat)
			continue
		}

		counts := map[string]int{}
		var mm minMax
		mm.kind = p.valueKind()
		var sum float64
		var sumN int
		var lenSum float64
		for _, row := range rows {
			v := row[p.valIdx]
			if v == nil {
				continue
			}
			stat.NonNull++
			s := valueString(v)
			counts[s]++
			mm.add(v, s)
			if p.valueKind() == kindNumeric {
				if f, ok := toFloat(v); ok {
					sum += f
					sumN++
				}
			}
			if p.role == roleText || (p.role == roleID && p.base == roleText) {
				lenSum += float64(utf8.RuneCountInString(s))
			}
		}
		stat.Distinct = len(counts)
		if key, ok := keySketchOf(p, col.Name, stat.NonNull, counts); ok {
			prof.keys = append(prof.keys, key)
		}

		// The mean of an identifier (e.g. a BIGINT primary key) carries no meaning, so only
		// ordinary numeric columns get one.
		if p.valueKind() == kindNumeric && sumN > 0 && p.role != roleID {
			stat.HasAvg = true
			stat.Avg = sum / float64(sumN)
		}
		if (p.role == roleText || (p.role == roleID && p.base == roleText)) && stat.NonNull > 0 {
			stat.HasAvg = true
			stat.AvgIsLength = true
			stat.Avg = lenSum / float64(stat.NonNull)
		}

		if !cfg.profileValues {
			stat.Hidden = true
			prof.Columns = append(prof.Columns, stat)
			continue
		}

		showRange := p.role == roleTime || p.role == roleNumeric ||
			(p.role == roleID && p.base == roleNumeric)
		if showRange && mm.set {
			stat.HasRange = true
			stat.Min, stat.Max = mm.minRaw, mm.maxRaw
		}

		// TopK only makes sense when values repeat in the sample. A near-unique column
		// (every value seen once or twice) would list noise, so it reports distinct count only.
		distinct := len(counts)
		repeats := distinct*2 <= stat.NonNull
		showTop := false
		switch p.role {
		case roleNumeric, roleText, roleBool:
			showTop = distinct > 0 && distinct <= categoricalMaxCard && repeats
		case roleID:
			showTop = distinct > 0 && distinct <= idTopKMaxCard && repeats
		}
		if showTop {
			stat.Top = topK(counts, stat.NonNull, cfg.topK)
		}
		prof.Columns = append(prof.Columns, stat)
	}
	return prof
}

// keySketchOf summarises a column for relation discovery when it can take part in a
// relation: identifiers, numeric columns and short text, with enough distinct values.
func keySketchOf(p colPlan, name string, nonNull int, counts map[string]int) (keySketch, bool) {
	switch p.role {
	case roleID, roleNumeric, roleText:
	default:
		return keySketch{}, false
	}
	if len(counts) < relMinDistinct {
		return keySketch{}, false
	}
	return buildKeySketch(name, p.valueKind() == kindNumeric, nonNull, counts), true
}

// topK returns the most frequent values, ties broken by value for determinism.
func topK(counts map[string]int, nonNull, k int) []valueShare {
	out := make([]valueShare, 0, len(counts))
	for v, c := range counts {
		share := 0.0
		if nonNull > 0 {
			share = float64(c) / float64(nonNull)
		}
		out = append(out, valueShare{Value: v, Count: c, Share: share})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Value < out[j].Value
	})
	if len(out) > k {
		out = out[:k]
	}
	return out
}

// minMax tracks the range of a column in its natural order.
type minMax struct {
	kind       valueKind
	set        bool
	minRaw     string
	maxRaw     string
	minF, maxF float64
	minT, maxT time.Time
	minS, maxS string
	hasT       bool
}

func (m *minMax) add(v interface{}, raw string) {
	switch m.kind {
	case kindNumeric:
		f, ok := toFloat(v)
		if !ok {
			return
		}
		if !m.set || f < m.minF {
			m.minF, m.minRaw = f, raw
		}
		if !m.set || f > m.maxF {
			m.maxF, m.maxRaw = f, raw
		}
	case kindTime:
		if t, ok := v.(time.Time); ok {
			if !m.hasT || t.Before(m.minT) {
				m.minT = t
				m.minRaw = t.Format(timeLayout)
			}
			if !m.hasT || t.After(m.maxT) {
				m.maxT = t
				m.maxRaw = t.Format(timeLayout)
			}
			m.hasT = true
			break
		}
		fallthrough
	default:
		if !m.set || raw < m.minS {
			m.minS, m.minRaw = raw, raw
		}
		if !m.set || raw > m.maxS {
			m.maxS, m.maxRaw = raw, raw
		}
	}
	m.set = true
}

func toFloat(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case int64:
		return float64(t), true
	case int32:
		return float64(t), true
	case int:
		return float64(t), true
	case uint64:
		return float64(t), true
	case uint32:
		return float64(t), true
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case []byte:
		f, err := strconv.ParseFloat(string(t), 64)
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

// valueString renders a driver value as stable display text.
func valueString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(t)
	case string:
		return t
	case time.Time:
		return t.Format(timeLayout)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

// truthy interprets a boolean-ish driver value (PostgreSQL bool, MySQL 0/1).
func truthy(v interface{}) bool {
	switch t := v.(type) {
	case bool:
		return t
	case int64:
		return t != 0
	case int32:
		return t != 0
	case []byte:
		return string(t) == "1"
	case string:
		return t == "1" || strings.EqualFold(t, "true")
	default:
		return false
	}
}
