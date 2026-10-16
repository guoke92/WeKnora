package repository

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrigramSimilarity_IdenticalAndDisjoint(t *testing.T) {
	q := trigramSet("租户互通产品表")
	assert.InDelta(t, 1.0, trigramSimilarity(q, "租户互通产品表"), 1e-9)
	assert.Equal(t, 0.0, trigramSimilarity(q, "zzzz"))
	assert.Equal(t, 0.0, trigramSimilarity(trigramSet(""), "租户"))
}

func TestTrigramSimilarity_CaseAndPunctuationInsensitive(t *testing.T) {
	q := trigramSet("Ca_Fee-Company")
	assert.InDelta(t, 1.0, trigramSimilarity(q, "ca fee company"), 1e-9)
}

func TestFindSimilarPages_SQLiteRecallsNearTitles(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()
	const kb = "kb-similar"

	pages := []*types.WikiPage{
		makeWikiPage(kb, "entity/ca-fee-company", types.WikiPageTypeEntity, types.WikiPageStatusPublished),
		makeWikiPage(kb, "entity/ca-fee-order", types.WikiPageTypeEntity, types.WikiPageStatusPublished),
		makeWikiPage(kb, "concept/column-profiling", types.WikiPageTypeConcept, types.WikiPageStatusPublished),
		makeWikiPage(kb, "entity/ca-fee-archived", types.WikiPageTypeEntity, types.WikiPageStatusArchived),
		makeWikiPage("other-kb", "entity/ca-fee-company-copy", types.WikiPageTypeEntity, types.WikiPageStatusPublished),
		makeWikiPage(kb, "summary/ca-fee-company", types.WikiPageTypeSummary, types.WikiPageStatusPublished),
	}
	for _, p := range pages {
		require.NoError(t, db.Create(p).Error)
	}

	got, err := repo.FindSimilarPages(ctx, kb, "ca-fee-company", nil, 5)
	require.NoError(t, err)
	require.NotEmpty(t, got)

	slugs := make([]string, 0, len(got))
	for _, p := range got {
		slugs = append(slugs, p.Slug)
	}
	assert.Equal(t, "entity/ca-fee-company", slugs[0], "exact title must rank first")
	assert.Contains(t, slugs, "entity/ca-fee-order")
	assert.NotContains(t, slugs, "entity/ca-fee-archived", "archived pages are not merge targets")
	assert.NotContains(t, slugs, "entity/ca-fee-company-copy", "pages of another knowledge base must not leak")
	assert.NotContains(t, slugs, "summary/ca-fee-company", "summary pages are outside the default entity+concept scope")
}

func TestFindSimilarPages_SQLiteRespectsLimitAndEmptyQuery(t *testing.T) {
	db := setupWikiPagesTestDB(t)
	repo := NewWikiPageRepository(db)
	ctx := context.Background()
	const kb = "kb-limit"
	for _, s := range []string{"entity/acme-a", "entity/acme-b", "entity/acme-c"} {
		require.NoError(t, db.Create(makeWikiPage(kb, s, types.WikiPageTypeEntity, types.WikiPageStatusPublished)).Error)
	}

	got, err := repo.FindSimilarPages(ctx, kb, "acme", nil, 2)
	require.NoError(t, err)
	assert.Len(t, got, 2)

	empty, err := repo.FindSimilarPages(ctx, kb, "   ", nil, 2)
	require.NoError(t, err)
	assert.Nil(t, empty)
}
