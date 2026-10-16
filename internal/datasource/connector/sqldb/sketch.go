package sqldb

import (
	"hash/fnv"
	"sort"
)

// Relation discovery (L3) compares columns through bottom-k MinHash sketches (also known
// as KMV). A sketch keeps the sketchSize smallest hashes of a column's distinct sampled
// values. Hashes are deterministic, so unchanged data yields identical sketches and
// identical documents, and the values themselves are never retained after profiling.

const (
	// sketchSize is the number of hashes kept per column. Columns with at most this many
	// distinct sampled values are represented exactly.
	sketchSize = 512
	// relMinDistinct is the number of distinct sampled values both columns of a relation
	// need. Smaller samples cannot distinguish a foreign key from a coincidence.
	relMinDistinct = 20
)

// keySketch is the relation-relevant summary of one column's sample.
type keySketch struct {
	Column   string
	Numeric  bool     // numeric columns only relate to numeric columns
	NonNull  int      // non-null sampled rows
	Distinct int      // exact distinct count within the sample
	Mins     []uint64 // the smallest hashes of the distinct values, ascending
}

// exact reports whether the sketch holds every distinct value.
func (k keySketch) exact() bool { return len(k.Mins) == k.Distinct }

// uniqueness is the share of non-null sampled rows that carry a distinct value.
func (k keySketch) uniqueness() float64 {
	if k.NonNull == 0 {
		return 0
	}
	return float64(k.Distinct) / float64(k.NonNull)
}

// buildKeySketch summarises the distinct values (counts keys) of one column sample.
func buildKeySketch(column string, numeric bool, nonNull int, counts map[string]int) keySketch {
	hashes := make([]uint64, 0, len(counts))
	for v := range counts {
		hashes = append(hashes, hashValue(v))
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i] < hashes[j] })
	// Equal hashes from distinct strings are astronomically unlikely, but keep the
	// sketch a set so the estimators stay well defined.
	hashes = dedupSorted(hashes)
	distinct := len(counts)
	if len(hashes) > sketchSize {
		hashes = hashes[:sketchSize]
	}
	return keySketch{Column: column, Numeric: numeric, NonNull: nonNull, Distinct: distinct, Mins: hashes}
}

// hashValue is FNV-1a with a splitmix finaliser; FNV alone has weak high bits for short
// strings, which would bias the bottom-k selection.
func hashValue(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	x := h.Sum64()
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

func dedupSorted(xs []uint64) []uint64 {
	if len(xs) < 2 {
		return xs
	}
	out := xs[:1]
	for _, x := range xs[1:] {
		if x != out[len(out)-1] {
			out = append(out, x)
		}
	}
	return out
}

// overlapEstimate returns the estimated size of the intersection and union of the
// distinct value sets behind two sketches. When both sketches are exact the numbers are
// exact; otherwise the bottom-k estimator is used (Beyer et al., 2007).
func overlapEstimate(a, b keySketch) (inter, union float64) {
	if a.exact() && b.exact() {
		common := commonCount(a.Mins, b.Mins)
		return float64(common), float64(a.Distinct + b.Distinct - common)
	}

	// Walk the union of the two sketches in ascending order, keeping the first sketchSize
	// values. A value seen in both sketches is a sample of the intersection.
	var kept, both int
	var lastHash uint64
	i, j := 0, 0
	for kept < sketchSize && (i < len(a.Mins) || j < len(b.Mins)) {
		var h uint64
		inA, inB := false, false
		switch {
		case j >= len(b.Mins) || (i < len(a.Mins) && a.Mins[i] < b.Mins[j]):
			h, inA = a.Mins[i], true
			i++
		case i >= len(a.Mins) || b.Mins[j] < a.Mins[i]:
			h, inB = b.Mins[j], true
			j++
		default:
			h, inA, inB = a.Mins[i], true, true
			i++
			j++
		}
		lastHash = h
		kept++
		if inA && inB {
			both++
		}
	}
	if kept == 0 {
		return 0, 0
	}

	// The k-th smallest hash, read as a fraction of the hash space, estimates the density
	// of the union: |A ∪ B| ≈ (k-1) / u_k.
	uk := (float64(lastHash) + 1) / (1 << 64)
	unionSize := float64(sketchSize-1) / uk
	if kept < sketchSize {
		unionSize = float64(kept)
	}
	jaccard := float64(both) / float64(kept)
	inter = jaccard * unionSize
	limit := float64(minInt(a.Distinct, b.Distinct))
	if inter > limit {
		inter = limit
	}
	union = float64(a.Distinct+b.Distinct) - inter
	return inter, union
}

func commonCount(a, b []uint64) int {
	n, i, j := 0, 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			n++
			i++
			j++
		}
	}
	return n
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
