package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
)

// Bounds for one external_db_query call. A query that reaches a bound is stopped and
// its result is marked truncated; the SQL itself is never rewritten.
const (
	externalDBQueryMaxRows  = 200
	externalDBQueryMaxBytes = 512 << 10
	externalDBQueryTimeout  = 60 * time.Second
)

var externalDBQueryTool = BaseTool{
	name: ToolExternalDBQuery,
	description: `Run one read-only SQL query against a MySQL or PostgreSQL data source that was synced into the knowledge base.

Target: pass knowledge_base_id (the bound knowledge base that holds the synced data source, from runtime_context). If that knowledge base has exactly one MySQL or PostgreSQL data source, it is used. If it has several, the error lists their datasource_id values; retry with datasource_id.

Get table and column names from the wiki pages first (wiki_search, wiki_read_page). Do not guess names.
- Only SELECT or WITH ... SELECT statements are accepted, and only over tables extracted from this data source.
- PostgreSQL tables must be schema-qualified (schema.table). MySQL tables may use bare names in the data source's database.
- Locking reads, SELECT INTO, multiple statements, and any write are rejected.
- A result is capped at 200 rows and 512 KiB and is marked truncated when it hits the cap. Summarize instead of listing every row.
- Results come from the live database. The synced documents are only a map of the schema, not a source of figures.`,
	schema: utils.GenerateSchema[ExternalDBQueryInput](),
}

// ExternalDBQueryInput is the argument of external_db_query. Exactly one of
// knowledge_base_id and datasource_id identifies the target; knowledge_base_id is the
// one the model normally has from runtime_context.
type ExternalDBQueryInput struct {
	KnowledgeBaseID string `json:"knowledge_base_id,omitempty" jsonschema:"ID of the bound knowledge base that holds the synced MySQL or PostgreSQL data source"`
	DataSourceID    string `json:"datasource_id,omitempty" jsonschema:"ID of a MySQL or PostgreSQL data source; needed only when the knowledge base has several"`
	SQL             string `json:"sql" jsonschema:"One read-only SELECT or WITH ... SELECT statement"`
}

// ExternalDBQueryTool runs checked read-only SQL against an external database source.
type ExternalDBQueryTool struct {
	BaseTool
	dataSources   interfaces.DataSourceService
	searchTargets types.SearchTargets
}

// NewExternalDBQueryTool creates the tool for one Agent turn. searchTargets limits which
// knowledge bases, and therefore which data sources, the tool may query.
func NewExternalDBQueryTool(dataSources interfaces.DataSourceService, searchTargets types.SearchTargets) *ExternalDBQueryTool {
	return &ExternalDBQueryTool{
		BaseTool:      externalDBQueryTool,
		dataSources:   dataSources,
		searchTargets: searchTargets,
	}
}

// Execute resolves the target data source, checks its knowledge base scope, runs the
// query through the data source service, and renders the bounded result as a Markdown
// table.
func (t *ExternalDBQueryTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var input ExternalDBQueryInput
	if err := json.Unmarshal(args, &input); err != nil {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf("Failed to parse args: %v", err)}, err
	}
	kbID := strings.TrimSpace(input.KnowledgeBaseID)
	dsID := strings.TrimSpace(input.DataSourceID)
	sqlText := strings.TrimSpace(input.SQL)
	if sqlText == "" || (kbID == "" && dsID == "") {
		return &types.ToolResult{Success: false, Error: "sql and knowledge_base_id (or datasource_id) are required"},
			fmt.Errorf("missing sql or target")
	}
	if t.dataSources == nil {
		return &types.ToolResult{Success: false, Error: "external database queries are unavailable"},
			fmt.Errorf("data source service is unavailable")
	}

	if dsID == "" {
		resolved, failure := t.resolveDataSource(ctx, kbID)
		if failure != nil {
			return failure, fmt.Errorf("%s", failure.Error)
		}
		dsID = resolved
	}

	ds, err := t.dataSources.GetDataSource(ctx, dsID)
	if err != nil || ds == nil {
		return &types.ToolResult{Success: false, Error: "data source not found"}, fmt.Errorf("data source %s not found", dsID)
	}
	if !isExternalDBSourceType(ds.Type) {
		return &types.ToolResult{Success: false, Error: "this data source is not a MySQL or PostgreSQL source"},
			fmt.Errorf("data source %s is not an external database", dsID)
	}
	if !t.searchTargets.ContainsKB(ds.KnowledgeBaseID) {
		return &types.ToolResult{Success: false,
				Error: "this data source is outside the knowledge bases of the current Agent scope"},
			fmt.Errorf("data source %s is outside the Agent scope", dsID)
	}

	logger.Infof(ctx, "[Tool][ExternalDBQuery] ds=%s sql_chars=%d", dsID, len([]rune(sqlText)))
	res, err := t.dataSources.RunExternalQuery(ctx, dsID, sqlText, types.ExternalQueryLimits{
		MaxRows:  externalDBQueryMaxRows,
		MaxBytes: externalDBQueryMaxBytes,
		Timeout:  externalDBQueryTimeout,
	})
	if err != nil {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf("Query failed: %v", err)}, err
	}

	return &types.ToolResult{
		Success: true,
		Output:  renderExternalQueryMarkdown(res),
		Data: map[string]interface{}{
			"display_type":  "external_db_query",
			"datasource_id": dsID,
			"columns":       res.Columns,
			"rows":          res.Rows,
			"row_count":     len(res.Rows),
			"truncated":     res.Truncated,
			"duration_ms":   res.Duration.Milliseconds(),
		},
	}, nil
}

// resolveDataSource maps a knowledge base to its single external database data source.
// When the knowledge base has none or several, it returns a failure result that the
// model can act on: the candidate IDs are listed for a retry with datasource_id.
func (t *ExternalDBQueryTool) resolveDataSource(ctx context.Context, kbID string) (string, *types.ToolResult) {
	if !t.searchTargets.ContainsKB(kbID) {
		return "", &types.ToolResult{Success: false,
			Error: "this knowledge base is outside the knowledge bases of the current Agent scope"}
	}
	list, err := t.dataSources.ListDataSources(ctx, kbID)
	if err != nil {
		return "", &types.ToolResult{Success: false, Error: "could not list data sources of this knowledge base"}
	}
	var candidates []*types.DataSource
	for _, ds := range list {
		if ds != nil && isExternalDBSourceType(ds.Type) {
			candidates = append(candidates, ds)
		}
	}
	switch len(candidates) {
	case 0:
		return "", &types.ToolResult{Success: false,
			Error: "this knowledge base has no MySQL or PostgreSQL data source"}
	case 1:
		return candidates[0].ID, nil
	}
	var b strings.Builder
	b.WriteString("This knowledge base has several MySQL or PostgreSQL data sources. Retry with datasource_id set to one of:")
	for _, ds := range candidates {
		fmt.Fprintf(&b, "\n- datasource_id=%s name=%q type=%s", ds.ID, ds.Name, ds.Type)
	}
	return "", &types.ToolResult{Success: false, Error: b.String()}
}

// isExternalDBSourceType reports whether a data source type is a MySQL or PostgreSQL
// database.
func isExternalDBSourceType(dsType string) bool {
	return dsType == types.ConnectorTypeMySQL || dsType == types.ConnectorTypePostgres
}

// renderExternalQueryMarkdown formats a result as a Markdown table, with a note when the
// result was cut short.
func renderExternalQueryMarkdown(res *types.ExternalQueryResult) string {
	var b strings.Builder
	if len(res.Columns) == 0 {
		return "The statement returned no columns."
	}
	fmt.Fprintf(&b, "%d row(s) returned", len(res.Rows))
	if res.Truncated {
		b.WriteString(" (truncated at the row or size limit; only the first rows are shown)")
	}
	b.WriteString(".\n\n")

	header := make([]string, len(res.Columns))
	rule := make([]string, len(res.Columns))
	for i, c := range res.Columns {
		header[i] = markdownCell(c)
		rule[i] = "---"
	}
	fmt.Fprintf(&b, "| %s |\n| %s |\n", strings.Join(header, " | "), strings.Join(rule, " | "))
	for _, row := range res.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = markdownCell(v)
		}
		fmt.Fprintf(&b, "| %s |\n", strings.Join(cells, " | "))
	}
	return b.String()
}

// markdownCell keeps a value on one table line.
func markdownCell(v string) string {
	r := strings.NewReplacer("|", "\\|", "\r\n", " ", "\n", " ", "\r", " ")
	return r.Replace(v)
}
