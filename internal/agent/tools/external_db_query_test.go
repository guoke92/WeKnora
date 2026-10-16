package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// fakeExternalDataSources implements only the two DataSourceService methods the tool uses.
type fakeExternalDataSources struct {
	interfaces.DataSourceService
	ds      *types.DataSource
	list    []*types.DataSource
	result  *types.ExternalQueryResult
	err     error
	queried bool
	gotSQL  string
	gotLim  types.ExternalQueryLimits
}

func (f *fakeExternalDataSources) ListDataSources(_ context.Context, _ string) ([]*types.DataSource, error) {
	return f.list, nil
}

func (f *fakeExternalDataSources) GetDataSource(_ context.Context, _ string) (*types.DataSource, error) {
	if f.ds == nil {
		return nil, errors.New("not found")
	}
	return f.ds, nil
}

func (f *fakeExternalDataSources) RunExternalQuery(
	_ context.Context, _ string, sqlText string, limits types.ExternalQueryLimits,
) (*types.ExternalQueryResult, error) {
	f.queried = true
	f.gotSQL = sqlText
	f.gotLim = limits
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func externalScope() types.SearchTargets {
	return types.SearchTargets{{
		Type:            types.SearchTargetTypeKnowledgeBase,
		KnowledgeBaseID: "kb-1",
	}}
}

func runExternalTool(t *testing.T, svc *fakeExternalDataSources, scope types.SearchTargets, args string) *types.ToolResult {
	t.Helper()
	tool := NewExternalDBQueryTool(svc, scope)
	res, _ := tool.Execute(context.Background(), json.RawMessage(args))
	if res == nil {
		t.Fatalf("Execute returned nil result")
	}
	return res
}

func TestExternalDBQueryRejectsDataSourceOutsideScope(t *testing.T) {
	svc := &fakeExternalDataSources{
		ds:     &types.DataSource{ID: "ds-1", Type: types.ConnectorTypeMySQL, KnowledgeBaseID: "kb-other"},
		result: &types.ExternalQueryResult{Columns: []string{"a"}},
	}
	res := runExternalTool(t, svc, externalScope(), `{"datasource_id":"ds-1","sql":"SELECT 1"}`)
	if res.Success {
		t.Fatalf("expected rejection for a data source outside the Agent scope")
	}
	if svc.queried {
		t.Fatalf("query must not run for a data source outside the Agent scope")
	}
}

func TestExternalDBQueryRejectsMissingDataSource(t *testing.T) {
	svc := &fakeExternalDataSources{}
	res := runExternalTool(t, svc, externalScope(), `{"datasource_id":"ds-x","sql":"SELECT 1"}`)
	if res.Success || svc.queried {
		t.Fatalf("expected failure without running the query when the data source is missing")
	}
}

func TestExternalDBQueryRequiresArguments(t *testing.T) {
	svc := &fakeExternalDataSources{ds: &types.DataSource{ID: "ds-1", Type: types.ConnectorTypeMySQL, KnowledgeBaseID: "kb-1"}}
	for _, args := range []string{`{"datasource_id":"","sql":"SELECT 1"}`, `{"datasource_id":"ds-1","sql":"  "}`} {
		res := runExternalTool(t, svc, externalScope(), args)
		if res.Success || svc.queried {
			t.Fatalf("args %s: expected failure before running the query", args)
		}
	}
}

func TestExternalDBQueryRendersMarkdownAndData(t *testing.T) {
	svc := &fakeExternalDataSources{
		ds: &types.DataSource{ID: "ds-1", Type: types.ConnectorTypeMySQL, KnowledgeBaseID: "kb-1"},
		result: &types.ExternalQueryResult{
			Columns: []string{"name", "note"},
			Rows: [][]string{
				{"a|b", "line1\nline2"},
				{"c", "NULL"},
			},
		},
	}
	res := runExternalTool(t, svc, externalScope(), `{"datasource_id":"ds-1","sql":"SELECT name, note FROM t"}`)
	if !res.Success {
		t.Fatalf("expected success, got error %q", res.Error)
	}
	if svc.gotSQL != "SELECT name, note FROM t" {
		t.Fatalf("unexpected SQL forwarded: %q", svc.gotSQL)
	}
	if svc.gotLim.MaxRows != externalDBQueryMaxRows || svc.gotLim.MaxBytes != externalDBQueryMaxBytes {
		t.Fatalf("limits not forwarded: %+v", svc.gotLim)
	}
	if !strings.Contains(res.Output, `| a\|b | line1 line2 |`) {
		t.Fatalf("pipe and newline must not break the table:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "truncated") {
		t.Fatalf("untruncated result must not carry a truncation note:\n%s", res.Output)
	}
	if res.Data["display_type"] != "external_db_query" {
		t.Fatalf("display_type = %v", res.Data["display_type"])
	}
	if res.Data["row_count"] != 2 || res.Data["truncated"] != false {
		t.Fatalf("unexpected data: row_count=%v truncated=%v", res.Data["row_count"], res.Data["truncated"])
	}
}

func TestExternalDBQueryMarksTruncatedResults(t *testing.T) {
	svc := &fakeExternalDataSources{
		ds: &types.DataSource{ID: "ds-1", Type: types.ConnectorTypeMySQL, KnowledgeBaseID: "kb-1"},
		result: &types.ExternalQueryResult{
			Columns:   []string{"id"},
			Rows:      [][]string{{"1"}},
			Truncated: true,
		},
	}
	res := runExternalTool(t, svc, externalScope(), `{"datasource_id":"ds-1","sql":"SELECT id FROM t"}`)
	if !res.Success {
		t.Fatalf("expected success, got error %q", res.Error)
	}
	if !strings.Contains(res.Output, "truncated") {
		t.Fatalf("truncated result must say so:\n%s", res.Output)
	}
	if res.Data["truncated"] != true {
		t.Fatalf("Data.truncated = %v", res.Data["truncated"])
	}
}

func TestExternalDBQueryReportsServiceErrors(t *testing.T) {
	svc := &fakeExternalDataSources{
		ds:  &types.DataSource{ID: "ds-1", Type: types.ConnectorTypeMySQL, KnowledgeBaseID: "kb-1"},
		err: errors.New("query rejected: write statement"),
	}
	res := runExternalTool(t, svc, externalScope(), `{"datasource_id":"ds-1","sql":"DELETE FROM t"}`)
	if res.Success {
		t.Fatalf("expected failure when the service rejects the query")
	}
	if !strings.Contains(res.Error, "write statement") {
		t.Fatalf("service error not surfaced: %q", res.Error)
	}
}

func TestExternalDBQueryResolvesSingleSourceFromKnowledgeBase(t *testing.T) {
	svc := &fakeExternalDataSources{
		ds: &types.DataSource{ID: "ds-1", Type: types.ConnectorTypePostgres, KnowledgeBaseID: "kb-1"},
		list: []*types.DataSource{
			{ID: "ds-1", Type: types.ConnectorTypePostgres, KnowledgeBaseID: "kb-1"},
			{ID: "ds-feishu", Type: types.ConnectorTypeFeishu, KnowledgeBaseID: "kb-1"},
		},
		result: &types.ExternalQueryResult{Columns: []string{"n"}, Rows: [][]string{{"1"}}},
	}
	res := runExternalTool(t, svc, externalScope(), `{"knowledge_base_id":"kb-1","sql":"SELECT 1"}`)
	if !res.Success {
		t.Fatalf("expected success, got error %q", res.Error)
	}
	if res.Data["datasource_id"] != "ds-1" {
		t.Fatalf("resolved datasource_id = %v, want ds-1", res.Data["datasource_id"])
	}
}

func TestExternalDBQueryListsCandidatesWhenKnowledgeBaseHasSeveralSources(t *testing.T) {
	svc := &fakeExternalDataSources{
		list: []*types.DataSource{
			{ID: "ds-a", Name: "orders", Type: types.ConnectorTypeMySQL, KnowledgeBaseID: "kb-1"},
			{ID: "ds-b", Name: "crm", Type: types.ConnectorTypePostgres, KnowledgeBaseID: "kb-1"},
		},
	}
	res := runExternalTool(t, svc, externalScope(), `{"knowledge_base_id":"kb-1","sql":"SELECT 1"}`)
	if res.Success || svc.queried {
		t.Fatalf("ambiguous target must fail before running the query")
	}
	if !strings.Contains(res.Error, "datasource_id=ds-a") || !strings.Contains(res.Error, "datasource_id=ds-b") {
		t.Fatalf("error must list candidate IDs so the model can retry: %q", res.Error)
	}
}

func TestExternalDBQueryRejectsKnowledgeBaseWithoutExternalSource(t *testing.T) {
	svc := &fakeExternalDataSources{
		list: []*types.DataSource{{ID: "ds-f", Type: types.ConnectorTypeFeishu, KnowledgeBaseID: "kb-1"}},
	}
	res := runExternalTool(t, svc, externalScope(), `{"knowledge_base_id":"kb-1","sql":"SELECT 1"}`)
	if res.Success || svc.queried {
		t.Fatalf("knowledge base without a database source must fail")
	}
}

func TestExternalDBQueryRejectsKnowledgeBaseOutsideScope(t *testing.T) {
	svc := &fakeExternalDataSources{
		list: []*types.DataSource{{ID: "ds-1", Type: types.ConnectorTypeMySQL, KnowledgeBaseID: "kb-other"}},
	}
	res := runExternalTool(t, svc, externalScope(), `{"knowledge_base_id":"kb-other","sql":"SELECT 1"}`)
	if res.Success || svc.queried {
		t.Fatalf("knowledge base outside the Agent scope must be rejected")
	}
}
