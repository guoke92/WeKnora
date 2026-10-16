package sqldb

import (
	"math"
	"strconv"
	"testing"
)

// seqRange returns the decimal strings lo..hi, each once.
func seqRange(lo, hi int) []string {
	out := make([]string, 0, hi-lo+1)
	for i := lo; i <= hi; i++ {
		out = append(out, strconv.Itoa(i))
	}
	return out
}

// sketchOfValues builds a numeric key sketch from one sampled value per row.
func sketchOfValues(name string, values []string) keySketch {
	counts := map[string]int{}
	for _, v := range values {
		counts[v]++
	}
	return buildKeySketch(name, true, len(values), counts)
}

func TestExactOverlapWhenBothSidesFit(t *testing.T) {
	a := sketchOfValues("a", seqRange(1, 30))
	b := sketchOfValues("b", seqRange(10, 50))
	if !a.exact() || !b.exact() {
		t.Fatalf("small columns should be exact")
	}
	inter, union := overlapEstimate(a, b)
	if inter != 21 || union != 50 {
		t.Fatalf("inter=%v union=%v, want 21 and 50", inter, union)
	}
}

func TestEstimatedContainmentTracksTruth(t *testing.T) {
	// A holds every fourth value of B: A is contained in B, |A ∩ B| = 2500.
	var sub []string
	for i := 1; i <= 10000; i += 4 {
		sub = append(sub, strconv.Itoa(i))
	}
	a := sketchOfValues("sub", sub)
	b := sketchOfValues("big", seqRange(1, 10000))
	if a.exact() || b.exact() {
		t.Fatalf("test needs sketches larger than sketchSize")
	}

	inter, _ := overlapEstimate(a, b)
	if rel := math.Abs(inter-2500) / 2500; rel > 0.15 {
		t.Fatalf("estimated intersection %.0f, want about 2500 (relative error %.2f)", inter, rel)
	}
	if cover := inter / float64(a.Distinct); cover < 0.85 {
		t.Fatalf("estimated containment of A in B = %.2f, want about 1", cover)
	}
}

func TestDisjointColumnsShareNothing(t *testing.T) {
	a := sketchOfValues("a", seqRange(1, 10000))
	b := sketchOfValues("b", seqRange(20001, 30000))
	inter, union := overlapEstimate(a, b)
	if inter != 0 {
		t.Fatalf("disjoint intersection = %v, want 0", inter)
	}
	if union <= 0 {
		t.Fatalf("union = %v, want positive", union)
	}
}

func TestSketchIsDeterministicAndBounded(t *testing.T) {
	values := seqRange(1, 5000)
	first := sketchOfValues("x", values)
	second := sketchOfValues("x", values)
	if len(first.Mins) != sketchSize {
		t.Fatalf("kept %d hashes, want %d", len(first.Mins), sketchSize)
	}
	for i := range first.Mins {
		if first.Mins[i] != second.Mins[i] {
			t.Fatalf("sketch differs at %d", i)
		}
		if i > 0 && first.Mins[i] <= first.Mins[i-1] {
			t.Fatalf("hashes not strictly ascending at %d", i)
		}
	}
	if first.Distinct != 5000 {
		t.Fatalf("distinct = %d, want 5000", first.Distinct)
	}
}

func TestUniquenessUsesNonNullRows(t *testing.T) {
	k := keySketch{NonNull: 10, Distinct: 9}
	if got := k.uniqueness(); math.Abs(got-0.9) > 1e-9 {
		t.Fatalf("uniqueness = %v, want 0.9", got)
	}
	if (keySketch{}).uniqueness() != 0 {
		t.Fatalf("empty sketch uniqueness should be 0")
	}
}
