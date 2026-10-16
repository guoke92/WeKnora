package sqldb

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/datasource"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

// fakeChat is a scripted chat model. reply sees the user message and returns the raw
// reply, or an error. It records every prompt it was given.
type fakeChat struct {
	reply  func(user string) (string, error)
	tokens int
	finish string
	calls  int
	users  []string
}

func (f *fakeChat) Chat(_ context.Context, messages []chat.Message, _ *chat.ChatOptions) (*types.ChatResponse, error) {
	f.calls++
	user := messages[len(messages)-1].Content
	f.users = append(f.users, user)
	text, err := f.reply(user)
	if err != nil {
		return nil, err
	}
	return &types.ChatResponse{
		Content:      text,
		FinishReason: f.finish,
		Usage:        types.TokenUsage{TotalTokens: f.tokens},
	}, nil
}

func (f *fakeChat) ChatStream(context.Context, []chat.Message, *chat.ChatOptions) (<-chan types.StreamResponse, error) {
	return nil, errors.New("streaming not used")
}
func (f *fakeChat) GetModelName() string { return "fake-model" }
func (f *fakeChat) GetModelID() string   { return "model-1" }

// ordersReply judges orders.buyer_id -> users.id as likely and adds one invalid column.
const ordersReply = `Here is the analysis:
{"summary": "Customer orders placed by buyers.",
 "columns": [{"name": "buyer_id", "meaning": "buyer user id"}, {"name": "ghost", "meaning": "not a column"}],
 "relations": [{"column": "buyer_id", "peer": "shop.users.id", "verdict": "likely", "reason": "buyer ids match user ids"},
               {"column": "buyer_id", "peer": "shop.nowhere.id", "verdict": "likely", "reason": "invented"}]}`

const plainReply = `{"summary": "Generic table.", "columns": [], "relations": []}`

func analysisCtx(model chat.Chat) context.Context {
	return datasource.WithAnalysis(context.Background(), datasource.Analysis{Model: model, Language: "Chinese"})
}

func runWith(t *testing.T, ctx context.Context, c *Connector, ds *types.DataSourceConfig, old *types.SyncCursor) (*recHandler, *types.SyncCursor) {
	t.Helper()
	h := &recHandler{}
	next, err := c.FetchStream(ctx, ds, old, h)
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	return h, next
}

func TestAnalysisAppliesValidatedVerdictsAndCaches(t *testing.T) {
	model := &fakeChat{reply: func(user string) (string, error) {
		if strings.Contains(user, "Name: shop.orders") {
			return ordersReply, nil
		}
		return plainReply, nil
	}}
	c := newTestConnector(newFakeRelationalShop())
	ds := shopDS(encodeSchemaID("shop"))
	h, cur := runWith(t, analysisCtx(model), c, ds, nil)

	orders := docOf(t, h, "orders")
	for _, want := range []string{
		"## 语义解读", "表用途：Customer orders placed by buyers.", "`buyer_id`：buyer user id",
		"语义判定：可能成立（buyer ids match user ids）",
	} {
		if !strings.Contains(orders, want) {
			t.Fatalf("orders document lacks %q:\n%s", want, orders)
		}
	}
	if strings.Contains(orders, "ghost") || strings.Contains(orders, "shop.nowhere.id") {
		t.Fatalf("invented column or relation reached the document:\n%s", orders)
	}
	if model.calls != 3 {
		t.Fatalf("model calls = %d, want one per table (3)", model.calls)
	}

	// The same data and model produce the same prompts: every result comes from the cache.
	before := model.calls
	again, _ := runWith(t, analysisCtx(model), c, ds, cur)
	if model.calls != before {
		t.Fatalf("unchanged tables called the model again (%d -> %d)", before, model.calls)
	}
	if got := again.emittedIDs(); len(got) != 0 {
		t.Fatalf("cached analysis re-emitted %v", got)
	}
}

func TestAnalysisPromptHasNoValuesAndNoSensitiveColumns(t *testing.T) {
	d := newFakeRelationalShop()
	orders := d.metas["shop.orders"]
	orders.Columns = append(orders.Columns,
		columnMeta{Name: "phone", DataType: "varchar", ColumnType: "varchar(20)", Comment: "contact phone"},
		columnMeta{Name: "remark", DataType: "varchar", ColumnType: "varchar(64)", Comment: "free text"})
	for i, row := range d.samples["shop.orders"] {
		row["phone"] = "13800001111"
		row["remark"] = "SECRETVALUE42"
		if i%2 == 0 {
			row["remark"] = "paid-in-full"
		}
	}

	model := &fakeChat{reply: func(string) (string, error) { return plainReply, nil }}
	runWith(t, analysisCtx(model), newTestConnector(d), shopDS(encodeSchemaID("shop")), nil)

	var ordersPrompt string
	for _, u := range model.users {
		if strings.Contains(u, "Name: shop.orders") {
			ordersPrompt = u
		}
	}
	if ordersPrompt == "" {
		t.Fatal("no prompt for orders")
	}
	for _, forbidden := range []string{"phone", "13800001111", "SECRETVALUE42", "paid-in-full", "contact phone"} {
		if strings.Contains(ordersPrompt, forbidden) {
			t.Fatalf("prompt leaks %q:\n%s", forbidden, ordersPrompt)
		}
	}
	if !strings.Contains(ordersPrompt, "- remark |") {
		t.Fatalf("ordinary columns should still be described:\n%s", ordersPrompt)
	}
}

func TestAnalysisRejectsUnusableOutput(t *testing.T) {
	model := &fakeChat{reply: func(string) (string, error) { return "I cannot answer in JSON today.", nil }}
	h, _ := runWith(t, analysisCtx(model), newTestConnector(newFakeRelationalShop()), shopDS(encodeSchemaID("shop")), nil)

	orders := docOf(t, h, "orders")
	if !strings.Contains(orders, "语义分析结果未通过校验") {
		t.Fatalf("rejected output should be stated in the document:\n%s", orders)
	}
	if strings.Contains(orders, "表用途") {
		t.Fatalf("rejected output must not produce a summary")
	}
}

// A reply cut at the output cap is reported as truncated, not as an invalid document.
// Truncation must not produce a summary even when the cut text looks like JSON.
func TestAnalysisTruncatedOutputIsReportedAsCut(t *testing.T) {
	model := &fakeChat{
		reply:  func(string) (string, error) { return `{"summary": "订单表", "columns": [`, nil },
		finish: "length",
	}
	h, _ := runWith(t, analysisCtx(model), newTestConnector(newFakeRelationalShop()), shopDS(encodeSchemaID("shop")), nil)

	orders := docOf(t, h, "orders")
	if !strings.Contains(orders, "输出超过长度上限被截断") {
		t.Fatalf("truncated output should be stated in the document:\n%s", orders)
	}
	if strings.Contains(orders, "表用途") {
		t.Fatal("truncated output must not produce a summary")
	}
}

func TestIsTruncatedRecognisesBothProviderSpellings(t *testing.T) {
	for _, r := range []string{"length", "max_tokens", " LENGTH "} {
		if !isTruncated(r) {
			t.Errorf("isTruncated(%q) = false, want true", r)
		}
	}
	for _, r := range []string{"", "stop", "end_turn", "tool_calls"} {
		if isTruncated(r) {
			t.Errorf("isTruncated(%q) = true, want false", r)
		}
	}
}

func TestAnalysisBudgetStopsModelCalls(t *testing.T) {
	model := &fakeChat{reply: func(string) (string, error) { return plainReply, nil }, tokens: 10}
	d := newFakeRelationalShop()
	ds := shopDS(encodeSchemaID("shop"))
	ds.Settings["llm_token_budget"] = float64(1)
	h, _ := runWith(t, analysisCtx(model), newTestConnector(d), ds, nil)

	if model.calls != 0 {
		t.Fatalf("model called %d times with a 1-token budget", model.calls)
	}
	if got := docOf(t, h, "orders"); !strings.Contains(got, "超出本次同步的 token 预算") {
		t.Fatalf("budget skip should be stated in the document:\n%s", got)
	}
}

func TestAnalysisFailuresDegradeWithoutFailingSync(t *testing.T) {
	model := &fakeChat{reply: func(string) (string, error) { return "", errors.New("upstream timeout") }}
	h, _ := runWith(t, analysisCtx(model), newTestConnector(newFakeRelationalShop()), shopDS(encodeSchemaID("shop")), nil)

	if len(h.emittedIDs()) != 3 {
		t.Fatalf("documents must still be emitted, got %v", h.emittedIDs())
	}
	if model.calls != analysisMaxFailures {
		t.Fatalf("model calls = %d, want %d before the circuit opens", model.calls, analysisMaxFailures)
	}
	if got := docOf(t, h, "orders"); !strings.Contains(got, "语义分析失败") {
		t.Fatalf("failure should be stated in the document:\n%s", got)
	}
	if strings.Contains(docOf(t, h, "orders"), "upstream timeout") {
		t.Fatal("driver or upstream detail leaked into the document")
	}
}

func TestAnalysisDisabledBySettingOrWithoutModel(t *testing.T) {
	model := &fakeChat{reply: func(string) (string, error) { return plainReply, nil }}
	ds := shopDS(encodeSchemaID("shop"))
	ds.Settings["llm_analysis"] = false
	h, _ := runWith(t, analysisCtx(model), newTestConnector(newFakeRelationalShop()), ds, nil)
	if model.calls != 0 {
		t.Fatalf("model called while disabled: %d", model.calls)
	}
	if strings.Contains(docOf(t, h, "orders"), "语义解读") {
		t.Fatal("semantic section rendered while disabled")
	}

	// Without a model on the context the step is simply absent.
	h, _ = runWith(t, context.Background(), newTestConnector(newFakeRelationalShop()), shopDS(encodeSchemaID("shop")), nil)
	if strings.Contains(docOf(t, h, "orders"), "语义解读") {
		t.Fatal("semantic section rendered without a model")
	}
}

func TestParseSemanticsKeepsOnlyCheckedFacts(t *testing.T) {
	allowed := map[string]bool{"buyer_id": true}
	candidates := map[string]bool{"buyer_id\x00shop.users.id": true}
	raw := "```json\n" + `{"summary": "  Orders\n of   buyers ",
		"columns": [{"name": "buyer_id", "meaning": "buyer"}, {"name": "buyer_id", "meaning": "dup"},
		            {"name": "other", "meaning": "x"}, {"name": "buyer_id", "meaning": ""}],
		"relations": [{"column": "buyer_id", "peer": "shop.users.id", "verdict": "LIKELY", "reason": "ok"},
		              {"column": "buyer_id", "peer": "shop.users.id", "verdict": "likely", "reason": "dup"},
		              {"column": "buyer_id", "peer": "shop.users.id", "verdict": "maybe", "reason": "bad verdict"}]}` + "\n```"

	out, dropped, err := parseSemantics(raw, allowed, candidates)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out.Summary != "Orders of buyers" {
		t.Fatalf("summary = %q", out.Summary)
	}
	if len(out.Columns) != 1 || out.Columns[0].Meaning != "buyer" {
		t.Fatalf("columns = %+v", out.Columns)
	}
	if len(out.Relations) != 1 || out.Relations[0].Verdict != "likely" {
		t.Fatalf("relations = %+v", out.Relations)
	}
	if dropped != 5 {
		t.Fatalf("dropped = %d, want 5", dropped)
	}
	if _, _, err := parseSemantics(`{"summary": "   "}`, allowed, candidates); err == nil {
		t.Fatal("empty summary should be rejected")
	}
}

func TestSemanticKeyChangesWithModelAndPrompt(t *testing.T) {
	base := semanticKey("sys", "user", "m1", "Chinese")
	if base != semanticKey("sys", "user", "m1", "Chinese") {
		t.Fatal("key must be stable")
	}
	for _, other := range []string{
		semanticKey("sys2", "user", "m1", "Chinese"),
		semanticKey("sys", "user2", "m1", "Chinese"),
		semanticKey("sys", "user", "m2", "Chinese"),
		semanticKey("sys", "user", "m1", "English"),
	} {
		if other == base {
			t.Fatal("key must change with any input")
		}
	}
}

func TestExtractJSONObjectRejectsTextWithoutObject(t *testing.T) {
	if _, err := extractJSONObject("no braces here"); err == nil {
		t.Fatal("expected an error")
	}
	got, err := extractJSONObject("prefix {\"a\": {\"b\": 1}} suffix")
	if err != nil || got != `{"a": {"b": 1}}` {
		t.Fatalf("got %q, %v", got, err)
	}
}
