package datasource

import (
	"context"

	"github.com/Tencent/WeKnora/internal/models/chat"
)

// Analysis is an optional model-assisted step (L4) that a connector may run during a
// sync. The sync service attaches it to the context when the knowledge base has a chat
// model; connectors without one, or without a context value, skip the step.
type Analysis struct {
	Model chat.Chat
	// PromptTemplate is the system prompt template. Empty selects the connector default.
	PromptTemplate string
	// Language names the language of the generated text, for example "Chinese".
	Language string
}

type analysisKey struct{}

// WithAnalysis attaches the analysis capability to ctx.
func WithAnalysis(ctx context.Context, a Analysis) context.Context {
	return context.WithValue(ctx, analysisKey{}, a)
}

// AnalysisFrom returns the analysis capability attached to ctx, if it has a model.
func AnalysisFrom(ctx context.Context) (Analysis, bool) {
	a, ok := ctx.Value(analysisKey{}).(Analysis)
	if !ok || a.Model == nil {
		return Analysis{}, false
	}
	return a, true
}
