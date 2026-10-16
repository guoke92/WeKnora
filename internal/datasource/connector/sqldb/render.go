package sqldb

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// renderTable turns a table description and its column profile into the Markdown
// document that enters the knowledge base as raw material for wiki compilation (P2).
//
// The output is deterministic for an unchanged table: no timestamps, row counts are
// bucketed to two significant digits, and every list is ordered. This is what lets the
// connector skip unchanged tables through the fingerprint in the cursor.
func renderTable(cfg config, meta *tableMeta, prof *tableProfile, profileNote string,
	rel *tableRelations, sem *semanticState) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s.%s\n\n", meta.Schema, meta.Name)
	fmt.Fprintf(&b, "- 数据源类型：%s\n", engineLabel(cfg.engine))
	fmt.Fprintf(&b, "- 数据库：%s\n", cfg.database)
	if meta.Schema != cfg.database {
		fmt.Fprintf(&b, "- Schema：%s\n", meta.Schema)
	}
	fmt.Fprintf(&b, "- 表注释：%s\n", orNone(meta.Comment))
	fmt.Fprintf(&b, "- 估算行数：%s\n", approxRows(meta.RowEstimate))
	fmt.Fprintf(&b, "- 主键：%s\n", keyList(meta.PrimaryKey))

	if sem != nil {
		writeSemantics(&b, sem)
	}

	b.WriteString("\n## 字段结构\n\n")
	b.WriteString("| 字段 | 类型 | 可空 | 默认值 | 注释 |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, c := range meta.Columns {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
			cell(c.Name), cell(c.ColumnType), yesNo(c.Nullable), cell(orNone(c.Default)), cell(orNone(c.Comment)))
	}

	b.WriteString("\n## 索引\n\n")
	if len(meta.Indexes) == 0 {
		b.WriteString("- 除主键外无其他索引\n")
	}
	for _, idx := range meta.Indexes {
		kind := "INDEX"
		if idx.Unique {
			kind = "UNIQUE INDEX"
		}
		fmt.Fprintf(&b, "- %s `%s` (%s)\n", kind, idx.Name, strings.Join(idx.Columns, ", "))
	}

	b.WriteString("\n## 声明的外键关系\n\n")
	if len(meta.ForeignKeys) == 0 {
		b.WriteString("- 无声明外键\n")
	}
	for _, fk := range meta.ForeignKeys {
		target := fk.RefTable
		if fk.RefSchema != "" && fk.RefSchema != meta.Schema {
			target = fk.RefSchema + "." + target
		}
		fmt.Fprintf(&b, "- `%s`: (%s) → %s (%s)\n",
			fk.Name, strings.Join(fk.Columns, ", "), target, strings.Join(fk.RefColumns, ", "))
	}

	if rel != nil {
		writeRelations(&b, rel)
	}

	if prof == nil && profileNote == "" {
		return b.String()
	}

	b.WriteString("\n## 列画像（基于样本）\n\n")
	if prof == nil {
		fmt.Fprintf(&b, "列画像暂不可用：%s\n", profileNote)
		return b.String()
	}
	fmt.Fprintf(&b, "- 样本行数：%d，采样方式：%s\n", prof.Sampled, prof.Mode)
	b.WriteString("- 数值与时间给出范围，低基数取值给出 TopK 占比，大字段仅给出非空率、平均长度与最近非空时间，敏感列只给出非空率\n")
	if prof.Skipped > 0 {
		fmt.Fprintf(&b, "- 另有 %d 个字段超出画像上限，未采集特征\n", prof.Skipped)
	}
	if profileNote != "" {
		fmt.Fprintf(&b, "- 注意：%s\n", profileNote)
	}
	b.WriteString("\n| 字段 | 角色 | 非空率 | 取值与范围 | 其他特征 |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, s := range prof.Columns {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
			cell(s.Name), roleLabel(s.Role), ratio(s.NonNull, s.Sampled),
			cell(valuesCell(s)), cell(extrasCell(s)))
	}

	hints := qualityHints(prof)
	if len(hints) > 0 {
		b.WriteString("\n## 数据质量提示\n\n")
		for _, h := range hints {
			fmt.Fprintf(&b, "- %s\n", h)
		}
	}
	return b.String()
}

// writeRelations renders the inferred relations of one table. The section is present
// whenever discovery ran, so an empty section means "none found" rather than "not checked".
func writeRelations(b *strings.Builder, rel *tableRelations) {
	b.WriteString("\n## 推断关系（基于样本估算，未在数据库中声明）\n\n")
	if rel.Skipped {
		fmt.Fprintf(b, "- 关系发现已跳过：参与比较的字段数超过上限（%d）\n", relMaxKeyColumns)
		return
	}
	if len(rel.Outgoing)+len(rel.Incoming)+len(rel.Overlaps) == 0 {
		b.WriteString("- 未发现推断关系\n")
		return
	}
	for _, l := range rel.Outgoing {
		// Relations are associations, not foreign keys: a shared key, a name match or a
		// status link all count. The wording states what was measured and nothing more.
		fmt.Fprintf(b, "- `%s` → `%s`：推断关联，本字段 %d 个去重取值中约 %.0f%% 出现在对端字段中（对端字段去重 %d）%s\n",
			cell(l.Column), cell(qualifiedColumn(l.Peer)), l.Distinct, l.Coverage*100, l.PeerDistinct, verdictSuffix(l))
	}
	for _, l := range rel.Incoming {
		fmt.Fprintf(b, "- `%s` ← `%s`：推断关联，对端约 %.0f%% 的去重取值出现在本字段中（对端字段去重 %d）%s\n",
			cell(l.Column), cell(qualifiedColumn(l.Peer)), l.Coverage*100, l.PeerDistinct, verdictSuffix(l))
	}
	for _, l := range rel.Overlaps {
		fmt.Fprintf(b, "- `%s` ↔ `%s`：取值重叠，估算 Jaccard 相似度 %.2f（本字段去重 %d，对端去重 %d）%s\n",
			cell(l.Column), cell(qualifiedColumn(l.Peer)), l.Jaccard, l.Distinct, l.PeerDistinct, verdictSuffix(l))
	}
	b.WriteString("- 以上依据样本估算，采样可能遗漏关联或产生误报，应结合字段含义确认\n")
}

// verdictSuffix renders the model's judgement of a relation, when there is one.
func verdictSuffix(l relationLine) string {
	if l.Verdict == "" {
		return ""
	}
	label := map[string]string{"likely": "可能成立", "unlikely": "可能是巧合", "uncertain": "不确定"}[l.Verdict]
	if l.Reason == "" {
		return fmt.Sprintf("；语义判定：%s", label)
	}
	return fmt.Sprintf("；语义判定：%s（%s）", label, cell(l.Reason))
}

// writeSemantics renders the model's reading of the table. Without a result it says why,
// so a missing reading is never mistaken for an empty one.
func writeSemantics(b *strings.Builder, sem *semanticState) {
	b.WriteString("\n## 语义解读（模型辅助，基于结构与统计，未使用取值）\n\n")
	if sem.Result == nil {
		fmt.Fprintf(b, "- %s\n", cell(sem.Note))
		return
	}
	fmt.Fprintf(b, "- 表用途：%s\n", cell(sem.Result.Summary))
	if len(sem.Result.Columns) > 0 {
		b.WriteString("- 字段含义：\n")
		for _, c := range sem.Result.Columns {
			fmt.Fprintf(b, "  - `%s`：%s\n", cell(c.Name), cell(c.Meaning))
		}
	}
}

// qualifiedColumn names a column of another table as schema.table.column.
func qualifiedColumn(e relationEnd) string {
	return e.Schema + "." + e.Table + "." + e.Column
}

func engineLabel(engine string) string {
	if engine == enginePostgres {
		return "PostgreSQL"
	}
	return "MySQL"
}

func roleLabel(r colRole) string {
	switch r {
	case roleID:
		return "标识"
	case roleTime:
		return "时间"
	case roleNumeric:
		return "数值"
	case roleBool:
		return "布尔"
	case roleText:
		return "文本"
	case roleLarge:
		return "大字段"
	case roleMasked:
		return "敏感(已脱敏)"
	default:
		return "其他"
	}
}

func valuesCell(s colStat) string {
	switch {
	case s.Role == roleMasked:
		return "（敏感列，不展示取值）"
	case s.Hidden:
		return "（未采集取值）"
	case s.Role == roleLarge:
		return "（大字段，不展示取值）"
	}
	var parts []string
	if s.HasRange {
		parts = append(parts, fmt.Sprintf("范围 %s ~ %s", s.Min, s.Max))
	}
	if len(s.Top) > 0 {
		shares := make([]string, 0, len(s.Top))
		for _, v := range s.Top {
			shares = append(shares, fmt.Sprintf("%s (%.1f%%)", truncate(displayValue(v.Value), topKDisplayRuneLimit), v.Share*100))
		}
		parts = append(parts, "TopK: "+strings.Join(shares, "; "))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, "；")
}

func extrasCell(s colStat) string {
	var parts []string
	if s.Distinct >= 0 && s.Role != roleMasked && s.Role != roleOther {
		parts = append(parts, fmt.Sprintf("样本内去重 %d", s.Distinct))
	}
	if s.HasAvg {
		if s.AvgIsLength {
			parts = append(parts, fmt.Sprintf("平均长度 %.0f", s.Avg))
		} else {
			parts = append(parts, fmt.Sprintf("平均值 %s", strconvFloat(s.Avg)))
		}
	}
	if s.Latest != "" {
		parts = append(parts, "最近非空时间 "+s.Latest)
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, "；")
}

// qualityHints lists conservative observations drawn from the sample only.
func qualityHints(prof *tableProfile) []string {
	var hints []string
	for _, s := range prof.Columns {
		if s.Sampled >= 20 && s.Role != roleOther {
			if ratioValue(s.NonNull, s.Sampled) < 0.05 {
				hints = append(hints, fmt.Sprintf("字段 `%s` 在样本中几乎全为空（非空率低于 5%%）", s.Name))
			}
		}
		if s.Role != roleMasked && s.Role != roleLarge && s.Distinct == 1 && s.NonNull > 1 {
			hints = append(hints, fmt.Sprintf("字段 `%s` 在样本中只有一个取值（可能是常量列）", s.Name))
		}
	}
	return hints
}

// approxRows rounds a row estimate to two significant digits so that small churn in the
// planner statistics does not change the rendered document.
func approxRows(n int64) string {
	if n < 0 {
		return "未知（尚未收集统计信息）"
	}
	if n < 100 {
		return fmt.Sprintf("%d", n)
	}
	magnitude := int64(1)
	for x := n; x >= 100; x /= 10 {
		magnitude *= 10
	}
	rounded := (n + magnitude/2) / magnitude * magnitude
	return fmt.Sprintf("约 %d", rounded)
}

func keyList(cols []string) string {
	if len(cols) == 0 {
		return "无"
	}
	return "(" + strings.Join(cols, ", ") + ")"
}

func ratio(nonNull, sampled int) string {
	if sampled == 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", float64(nonNull)*100/float64(sampled))
}

func ratioValue(nonNull, sampled int) float64 {
	if sampled == 0 {
		return 0
	}
	return float64(nonNull) / float64(sampled)
}

func yesNo(v bool) string {
	if v {
		return "是"
	}
	return "否"
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "无"
	}
	return s
}

// displayValue makes an empty string visible in TopK lists.
func displayValue(v string) string {
	if v == "" {
		return "（空字符串）"
	}
	return v
}

func truncate(s string, runes int) string {
	if utf8.RuneCountInString(s) <= runes {
		return s
	}
	r := []rune(s)
	return string(r[:runes]) + "…"
}

// cell escapes a value for a Markdown table cell.
func cell(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ", "|", `\|`).Replace(s)
	return strings.TrimSpace(s)
}

func strconvFloat(f float64) string {
	return fmt.Sprintf("%.4g", f)
}
