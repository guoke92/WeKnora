package repository

import (
	"context"
	"sort"
	"strings"
	"unicode"

	"github.com/Tencent/WeKnora/internal/types"
)

// sqliteSimilarityThreshold mirrors the recall floor documented on
// FindSimilarPages. The dedup pass applies its own, stricter scoring after
// candidates are returned, so this only decides what is worth scoring.
const sqliteSimilarityThreshold = 0.1

// sqliteSimilarScanLimit bounds how many entity/concept rows one probe scores.
// A single knowledge base's entity+concept pages are far below this on Lite;
// the cap only protects against pathological knowledge bases.
const sqliteSimilarScanLimit = 20000

// findSimilarPagesSQLite is the SQLite (Lite) counterpart of the pg_trgm query
// in FindSimilarPages. SQLite has no similarity() or % operator, so without
// this path every dedup probe fails with "no such function: similarity" and
// duplicate entity/concept pages are never offered to the merge judge.
//
// The knowledge base's candidate rows are scored in Go with the same trigram
// model pg_trgm uses, so the ranking matches the Postgres path on the title.
func (r *wikiPageRepository) findSimilarPagesSQLite(
	ctx context.Context,
	kbID string,
	q string,
	pageTypes []string,
	limit int,
) ([]*types.WikiPageLite, error) {
	var rows []types.WikiPageLite
	if err := r.db.WithContext(ctx).
		Model(&types.WikiPage{}).
		Select("slug, title, page_type, status, aliases, out_links").
		Where("knowledge_base_id = ? AND page_type IN ? AND status <> ?",
			kbID, pageTypes, types.WikiPageStatusArchived).
		Limit(sqliteSimilarScanLimit).
		Scan(&rows).Error; err != nil {
		return nil, err
	}

	queryGrams := trigramSet(q)
	type scored struct {
		row types.WikiPageLite
		sim float64
	}
	hits := make([]scored, 0, len(rows))
	for i := range rows {
		sim := trigramSimilarity(queryGrams, rows[i].Title)
		if sim < sqliteSimilarityThreshold {
			continue
		}
		hits = append(hits, scored{row: rows[i], sim: sim})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].sim > hits[j].sim })
	if len(hits) > limit {
		hits = hits[:limit]
	}

	out := make([]*types.WikiPageLite, len(hits))
	for i := range hits {
		row := hits[i].row
		out[i] = &row
	}
	return out, nil
}

// trigramSet returns the pg_trgm-style trigram set of s: runs of letters or
// digits are words, each word is padded with two leading and one trailing
// space, and every 3-rune window is a gram. Works for CJK text too, because
// CJK ideographs are letters.
func trigramSet(s string) map[string]struct{} {
	grams := make(map[string]struct{})
	var word []rune
	flush := func() {
		if len(word) == 0 {
			return
		}
		padded := make([]rune, 0, len(word)+3)
		padded = append(padded, ' ', ' ')
		padded = append(padded, word...)
		padded = append(padded, ' ')
		for i := 0; i+3 <= len(padded); i++ {
			grams[string(padded[i:i+3])] = struct{}{}
		}
		word = word[:0]
	}
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			word = append(word, r)
			continue
		}
		flush()
	}
	flush()
	return grams
}

// trigramSimilarity is the Jaccard similarity of the query's trigram set and
// the trigram set of candidate, the same measure pg_trgm's similarity() uses.
func trigramSimilarity(queryGrams map[string]struct{}, candidate string) float64 {
	candGrams := trigramSet(candidate)
	if len(queryGrams) == 0 || len(candGrams) == 0 {
		return 0
	}
	shared := 0
	for g := range candGrams {
		if _, ok := queryGrams[g]; ok {
			shared++
		}
	}
	union := len(queryGrams) + len(candGrams) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}
