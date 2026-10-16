package sqldb

import (
	"sort"
)

// Inferred relations (L3). Foreign keys that the database does not declare are found by
// comparing the sampled values of key-like columns. The verdicts are conservative and
// only report evidence numbers, never values.

const (
	relKindFK      = "inferred_fk" // From's values are contained in To, which is key-like
	relKindOverlap = "overlap"     // the value sets overlap heavily; direction unknown

	relFKContainment  = 0.9 // share of From's distinct values that must appear in To
	relKeyUniqueness  = 0.9 // To must carry (almost) one row per value
	relOverlapJaccard = 0.5 // estimated Jaccard similarity for an overlap

	// A referenced column needs enough distinct values for containment to mean something:
	// a 35-value column contains most small integers by chance.
	relFKTargetMinDistinct = 100
	// Overlaps between small value sets (tenant codes, flags) are mostly coincidence.
	relOverlapMinDistinct = 50

	// relMaxBucket drops sketch values shared by many columns: those are generic values
	// (0, empty string, a status code) and would link unrelated columns.
	relMaxBucket = 32
	// relMaxKeyColumns bounds the comparison work; larger selections skip relation
	// discovery and say so in the documents.
	relMaxKeyColumns = 2000
)

// relTable is one profiled table as seen by relation discovery.
type relTable struct {
	ID     string
	Schema string
	Name   string
	Meta   *tableMeta
	Keys   []keySketch
}

// relationEnd is one column of a relation.
type relationEnd struct {
	TableID string
	Schema  string
	Table   string
	Column  string
}

// relationFinding is one inferred relation with the evidence that produced it.
type relationFinding struct {
	Kind         string
	From         relationEnd // inferred_fk: the referencing column; overlap: one side
	To           relationEnd // inferred_fk: the referenced column; overlap: the other side
	Coverage     float64     // share of From's distinct values found in To (estimated)
	Jaccard      float64     // estimated Jaccard similarity of the two value sets
	FromDistinct int
	ToDistinct   int
}

// flatKey is a key sketch with the index of its table in relTable order.
type flatKey struct {
	tbl int
	key keySketch
}

// discoverRelations returns the inferred relations among the given tables, sorted for
// deterministic documents. skipped reports that the selection was too large to compare.
func discoverRelations(tables []relTable) (findings []relationFinding, skipped bool) {
	var cols []flatKey
	for ti, t := range tables {
		for _, k := range t.Keys {
			if k.Distinct >= relMinDistinct && k.NonNull >= relMinDistinct {
				cols = append(cols, flatKey{tbl: ti, key: k})
			}
		}
	}
	if len(cols) < 2 {
		return nil, false
	}
	if len(cols) > relMaxKeyColumns {
		return nil, true
	}

	// Candidate pairs share a sketch value. Each sketch value is a one-row LSH band, so a
	// referencing column that is a subset of its target still shares values with it.
	buckets := map[uint64][]int{}
	for i, c := range cols {
		for _, h := range c.key.Mins {
			buckets[h] = append(buckets[h], i)
		}
	}
	type pair struct{ a, b int }
	candidates := map[pair]bool{}
	for _, members := range buckets {
		if len(members) < 2 || len(members) > relMaxBucket {
			continue
		}
		for x := 0; x < len(members); x++ {
			for y := x + 1; y < len(members); y++ {
				a, b := members[x], members[y]
				if canRelate(cols[a], cols[b]) {
					candidates[pair{a, b}] = true
				}
			}
		}
	}
	ordered := make([]pair, 0, len(candidates))
	for p := range candidates {
		ordered = append(ordered, p)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].a != ordered[j].a {
			return ordered[i].a < ordered[j].a
		}
		return ordered[i].b < ordered[j].b
	})

	for _, p := range ordered {
		if f, ok := judgePair(tables, cols[p.a], cols[p.b]); ok {
			findings = append(findings, f)
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.From.TableID != b.From.TableID {
			return a.From.TableID < b.From.TableID
		}
		if a.From.Column != b.From.Column {
			return a.From.Column < b.From.Column
		}
		if a.To.TableID != b.To.TableID {
			return a.To.TableID < b.To.TableID
		}
		return a.To.Column < b.To.Column
	})
	return findings, false
}

// canRelate reports whether two columns can relate: same value class, and not the
// same column of the same table.
func canRelate(a, b flatKey) bool {
	if a.key.Numeric != b.key.Numeric {
		return false
	}
	return !(a.tbl == b.tbl && a.key.Column == b.key.Column)
}

// judgePair applies the conservative verdicts to one candidate pair. Pairs already
// declared as foreign keys in either direction are left to the declared section.
func judgePair(tables []relTable, a, b flatKey) (relationFinding, bool) {
	inter, union := overlapEstimate(a.key, b.key)
	if union <= 0 || inter <= 0 {
		return relationFinding{}, false
	}
	coverAB := inter / float64(a.key.Distinct) // share of A's values found in B
	coverBA := inter / float64(b.key.Distinct) // share of B's values found in A
	jaccard := inter / union

	ea, eb := endOf(tables, a), endOf(tables, b)
	if declaresForeignKey(tables[a.tbl].Meta, ea, eb) || declaresForeignKey(tables[b.tbl].Meta, eb, ea) {
		return relationFinding{}, false
	}

	aRefsB := isReferencedKey(b.key) && coverAB >= relFKContainment
	bRefsA := isReferencedKey(a.key) && coverBA >= relFKContainment
	base := relationFinding{
		Jaccard:      jaccard,
		FromDistinct: a.key.Distinct,
		ToDistinct:   b.key.Distinct,
		From:         ea,
		To:           eb,
	}
	switch {
	case aRefsB && bRefsA:
		// Both sets are contained in each other: equal key sets, so no direction.
		base.Kind = relKindOverlap
		return base, true
	case aRefsB:
		base.Kind, base.Coverage = relKindFK, coverAB
		return base, true
	case bRefsA:
		base.Kind, base.Coverage = relKindFK, coverBA
		base.From, base.To = eb, ea
		base.FromDistinct, base.ToDistinct = b.key.Distinct, a.key.Distinct
		return base, true
	case jaccard >= relOverlapJaccard && a.key.Distinct >= relOverlapMinDistinct && b.key.Distinct >= relOverlapMinDistinct:
		base.Kind = relKindOverlap
		return base, true
	}
	return relationFinding{}, false
}

// isReferencedKey reports whether a column can be the referenced side of a foreign key:
// nearly one row per value and enough distinct values.
func isReferencedKey(k keySketch) bool {
	return k.uniqueness() >= relKeyUniqueness && k.Distinct >= relFKTargetMinDistinct
}

func endOf(tables []relTable, c flatKey) relationEnd {
	t := tables[c.tbl]
	return relationEnd{TableID: t.ID, Schema: t.Schema, Table: t.Name, Column: c.key.Column}
}

// declaresForeignKey reports whether the table of from declares the same single-column
// foreign key that the finding proposes.
func declaresForeignKey(meta *tableMeta, from, to relationEnd) bool {
	if meta == nil {
		return false
	}
	for _, fk := range meta.ForeignKeys {
		if len(fk.Columns) != 1 || len(fk.RefColumns) != 1 {
			continue
		}
		if fk.Columns[0] != from.Column || fk.RefColumns[0] != to.Column || fk.RefTable != to.Table {
			continue
		}
		refSchema := fk.RefSchema
		if refSchema == "" {
			refSchema = meta.Schema
		}
		if refSchema == to.Schema {
			return true
		}
	}
	return false
}

// tableRelations are the inferred relations that involve one table.
type tableRelations struct {
	Skipped  bool // discovery did not run because the selection is too large
	Outgoing []relationLine
	Incoming []relationLine
	Overlaps []relationLine
}

// relationLine is one relation seen from one of its tables.
type relationLine struct {
	Column       string      // the column of this table
	Peer         relationEnd // the other side
	Coverage     float64     // inferred_fk: share of the referencing side's values found in the referenced side
	Jaccard      float64     // overlap
	Distinct     int         // distinct sampled values of this column
	PeerDistinct int         // distinct sampled values of the peer column
	Kind         string

	// Verdict and Reason are the model's judgement (L4), empty when none was made.
	Verdict string
	Reason  string
}

// relationsByTable splits findings per table. Every id in tableIDs gets an entry, so a
// document can tell "no relations found" from "discovery did not run".
func relationsByTable(findings []relationFinding, tableIDs []string, skipped bool) map[string]*tableRelations {
	out := make(map[string]*tableRelations, len(tableIDs))
	for _, id := range tableIDs {
		out[id] = &tableRelations{Skipped: skipped}
	}
	if skipped {
		return out
	}
	for _, f := range findings {
		from, to := out[f.From.TableID], out[f.To.TableID]
		switch f.Kind {
		case relKindFK:
			if from != nil {
				from.Outgoing = append(from.Outgoing, relationLine{
					Kind: f.Kind, Column: f.From.Column, Peer: f.To, Coverage: f.Coverage,
					Distinct: f.FromDistinct, PeerDistinct: f.ToDistinct,
				})
			}
			if to != nil {
				to.Incoming = append(to.Incoming, relationLine{
					Kind: f.Kind, Column: f.To.Column, Peer: f.From, Coverage: f.Coverage,
					Distinct: f.ToDistinct, PeerDistinct: f.FromDistinct,
				})
			}
		case relKindOverlap:
			if from != nil {
				from.Overlaps = append(from.Overlaps, relationLine{
					Kind: f.Kind, Column: f.From.Column, Peer: f.To, Jaccard: f.Jaccard,
					Distinct: f.FromDistinct, PeerDistinct: f.ToDistinct,
				})
			}
			if to != nil {
				to.Overlaps = append(to.Overlaps, relationLine{
					Kind: f.Kind, Column: f.To.Column, Peer: f.From, Jaccard: f.Jaccard,
					Distinct: f.ToDistinct, PeerDistinct: f.FromDistinct,
				})
			}
		}
	}
	for _, rel := range out {
		sortLines(rel.Outgoing)
		sortLines(rel.Incoming)
		sortLines(rel.Overlaps)
	}
	return out
}

func sortLines(lines []relationLine) {
	sort.SliceStable(lines, func(i, j int) bool {
		if lines[i].Column != lines[j].Column {
			return lines[i].Column < lines[j].Column
		}
		pi, pj := lines[i].Peer, lines[j].Peer
		if pi.Schema != pj.Schema {
			return pi.Schema < pj.Schema
		}
		if pi.Table != pj.Table {
			return pi.Table < pj.Table
		}
		return pi.Column < pj.Column
	})
}
