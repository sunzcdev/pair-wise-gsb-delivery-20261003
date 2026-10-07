package geo

import (
	"math"
	"testing"

	"github.com/paulmach/orb"
)

func TestSplitAntimeridianShortLineStrings(t *testing.T) {
	if got := SplitAntimeridian(nil); len(got) != 1 || len(got[0]) != 0 {
		t.Fatalf("nil input should produce one empty piece, got %v", got)
	}

	single := orb.LineString{{179, 10}}
	got := SplitAntimeridian(single)
	if len(got) != 1 || !got[0].Equal(single) {
		t.Fatalf("single point mismatch: %v", got)
	}

	// The input must not be modified.
	if single[0] != (orb.Point{179, 10}) {
		t.Fatalf("input was modified: %v", single)
	}
}

func TestSplitAntimeridianNoCrossing(t *testing.T) {
	line := orb.LineString{{-10, 0}, {0, 1}, {10, 0}}
	got := SplitAntimeridian(line)
	if len(got) != 1 {
		t.Fatalf("expected one piece, got %d: %v", len(got), got)
	}
	if !got[0].Equal(line) {
		t.Fatalf("expected %v, got %v", line, got[0])
	}
}

func TestSplitAntimeridianExactly180(t *testing.T) {
	// A difference of exactly 180 degrees is the tie case and must not be cut.
	line := orb.LineString{{90, 0}, {-90, 5}}
	got := SplitAntimeridian(line)
	if len(got) != 1 {
		t.Fatalf("exactly 180 degrees must not be split, got %v", got)
	}
	if !got[0].Equal(line) {
		t.Fatalf("expected %v, got %v", line, got[0])
	}
}

func TestSplitAntimeridianEastwardCrossing(t *testing.T) {
	// 179 -> -179 is a two degree step to the east across the seam.
	line := orb.LineString{{179, 0}, {-179, 2}}
	got := SplitAntimeridian(line)

	expected := orb.MultiLineString{
		{{179, 0}, {180, 1}},
		{{-180, 1}, {-179, 2}},
	}
	if !multiEqual(got, expected) {
		t.Fatalf("expected %v, got %v", expected, got)
	}
	assertInRange(t, got)
}

func TestSplitAntimeridianWestwardCrossing(t *testing.T) {
	// -179 -> 179 is a two degree step to the west across the seam.
	line := orb.LineString{{-179, 0}, {179, 2}}
	got := SplitAntimeridian(line)

	expected := orb.MultiLineString{
		{{-179, 0}, {-180, 1}},
		{{180, 1}, {179, 2}},
	}
	if !multiEqual(got, expected) {
		t.Fatalf("expected %v, got %v", expected, got)
	}
	assertInRange(t, got)
}

func TestSplitAntimeridianMultipleCrossings(t *testing.T) {
	// Cross east, wander, then cross east again. The intermediate step
	// -80 -> 100 is only 180 degrees, the tie case, and does not cross.
	line := orb.LineString{{170, 0}, {-170, 1}, {-100, 2}, {-90, 3}, {-80, 2}, {100, 1}}
	got := SplitAntimeridian(line)

	if len(got) != 2 {
		t.Fatalf("expected two pieces, got %d: %v", len(got), got)
	}
	assertInRange(t, got)
	assertReunwrapped(t, line, got)
}

func TestSplitAntimeridianVertexOnSeam(t *testing.T) {
	t.Run("stays on one side", func(t *testing.T) {
		line := orb.LineString{{170, 0}, {180, 1}, {170, 2}}
		got := SplitAntimeridian(line)
		expected := orb.MultiLineString{
			{{170, 0}, {180, 1}, {170, 2}},
		}
		if !multiEqual(got, expected) {
			t.Fatalf("expected %v, got %v", expected, got)
		}
	})

	t.Run("crosses through single seam vertex", func(t *testing.T) {
		line := orb.LineString{{170, 0}, {180, 1}, {-170, 2}}
		got := SplitAntimeridian(line)
		expected := orb.MultiLineString{
			{{170, 0}, {180, 1}},
			{{-180, 1}, {-170, 2}},
		}
		if !multiEqual(got, expected) {
			t.Fatalf("expected %v, got %v", expected, got)
		}
		assertInRange(t, got)
	})

	t.Run("crosses through run of seam vertices", func(t *testing.T) {
		line := orb.LineString{{170, 0}, {180, 1}, {-180, 2}, {180, 3}, {-170, 4}}
		got := SplitAntimeridian(line)
		expected := orb.MultiLineString{
			{{170, 0}, {180, 1}, {180, 2}, {180, 3}},
			{{-180, 3}, {-170, 4}},
		}
		if !multiEqual(got, expected) {
			t.Fatalf("expected %v, got %v", expected, got)
		}
		assertInRange(t, got)
	})
}

func TestSplitAntimeridianDoesNotMutateInput(t *testing.T) {
	line := orb.LineString{{170, 0}, {180, 1}, {-170, 2}, {170, 3}}
	original := line.Clone()
	_ = SplitAntimeridian(line)
	if !line.Equal(original) {
		t.Fatalf("input modified: want %v got %v", original, line)
	}
}

func TestSplitAntimeridianNoSinglePointPieces(t *testing.T) {
	lines := []orb.LineString{
		{{179, 0}, {-179, 1}},
		{{-179, 0}, {179, 1}},
		{{170, 0}, {180, 1}, {-170, 2}, {170, 3}},
		{{180, 0}, {-179, 1}},
		{{179, 0}, {-180, 1}},
	}
	for _, line := range lines {
		got := SplitAntimeridian(line)
		for i, piece := range got {
			if len(piece) < 2 {
				t.Fatalf("piece %d of %v is a single point: %v", i, line, piece)
			}
		}
		assertReunwrapped(t, line, got)
	}
}

func TestSplitAntimeridianPreservesPath(t *testing.T) {
	lines := []orb.LineString{
		{{0, 0}, {10, 1}, {20, 0}},
		{{179, 0}, {-179, 2}, {-100, 3}},
		{{-179, 0}, {179, -2}, {100, -3}},
		{{170, 0}, {-170, 1}, {170, 2}, {-170, 3}, {170, 4}},
		{{180, 0}, {170, 1}, {180, 2}, {-170, 3}},
		{{-180, 5}, {179.5, 4}, {-179.5, 6}},
		{{90, 0}, {-90, 5}},
		{{45, 10}, {-135, -10}, {45, -20}},
	}
	for _, line := range lines {
		got := SplitAntimeridian(line)
		assertInRange(t, got)
		assertReunwrapped(t, line, got)
	}
}

// assertInRange verifies every emitted longitude lies within [-180, 180].
func assertInRange(t *testing.T, mls orb.MultiLineString) {
	t.Helper()
	for _, piece := range mls {
		for _, p := range piece {
			if p[0] < -180 || p[0] > 180 {
				t.Fatalf("longitude %v out of range in %v", p, mls)
			}
		}
	}
}

// assertReunwrapped expands the cut pieces back into a continuous longitude
// domain and confirms the original vertices appear, in order, and that the
// total linear length matches the original unwrapped trajectory.
func assertReunwrapped(t *testing.T, line orb.LineString, mls orb.MultiLineString) {
	t.Helper()

	original := unwrapOriginal(line)

	var rebuilt []orb.Point
	for pi, piece := range mls {
		var offset float64
		switch {
		case pi == 0:
			// Anchor the first piece using its first strictly off-seam vertex,
			// which uniquely determines the continuous cell.
			offset = 0
			for i, p := range piece {
				if !isSeam(p[0]) {
					offset = original[i][0] - p[0]
					break
				}
			}
		default:
			// Later pieces begin at the same seam point where the previous
			// piece ended; translate the whole piece to line up exactly.
			prev := rebuilt[len(rebuilt)-1]
			offset = prev[0] - piece[0][0]
		}

		for i, p := range piece {
			if pi > 0 && i == 0 {
				rebuilt = append(rebuilt, rebuilt[len(rebuilt)-1])
				continue
			}
			rebuilt = append(rebuilt, orb.Point{p[0] + offset, p[1]})
		}
	}

	// The seam may be represented by +180/-180 duplicates, but every original
	// vertex must appear in the rebuilt path in order.
	idx := 0
	for _, want := range original {
		for idx < len(rebuilt) {
			cand := rebuilt[idx]
			idx++
			if pointsClose(cand, want) {
				break
			}
			if idx == len(rebuilt) {
				t.Fatalf("original vertex %v not found in order in rebuilt %v", want, rebuilt)
			}
		}
	}

	wantLength := polylineLength(original)
	gotLength := polylineLength(rebuilt)
	if math.Abs(wantLength-gotLength) > 1e-9 {
		t.Fatalf("length mismatch: want %v got %v for line %v", wantLength, gotLength, line)
	}
}

// unwrapOriginal produces the shortest-path continuous longitude version of
// the input, matching the interpretation used by SplitAntimeridian.
func unwrapOriginal(line orb.LineString) []orb.Point {
	if len(line) == 0 {
		return nil
	}
	out := make([]orb.Point, len(line))
	out[0] = line[0]
	if isSeam(out[0][0]) {
		out[0][0] = 180
	}
	for i := 1; i < len(line); i++ {
		delta := line[i][0] - line[i-1][0]
		if isSeam(line[i-1][0]) {
			delta = line[i][0] - out[i-1][0]
		}
		if delta > 180 {
			delta -= 360
		} else if delta < -180 {
			delta += 360
		}
		lon := out[i-1][0] + delta
		if isSeam(line[i][0]) {
			lon = math.Round((lon+180)/360)*360 - 180
		}
		out[i] = orb.Point{lon, line[i][1]}
	}
	return out
}

func polylineLength(points []orb.Point) float64 {
	sum := 0.0
	for i := 1; i < len(points); i++ {
		sum += math.Hypot(points[i][0]-points[i-1][0], points[i][1]-points[i-1][1])
	}
	return sum
}

func pointsClose(a, b orb.Point) bool {
	const eps = 1e-9
	return lonClose(a[0], b[0]) && math.Abs(a[1]-b[1]) <= eps
}

func isSeam(lon float64) bool {
	return lon == 180 || lon == -180
}

// lonClose treats +180 and -180 as the same seam longitude.
func lonClose(a, b float64) bool {
	if math.Abs(a-b) <= 1e-9 {
		return true
	}
	return math.Abs(math.Abs(a)-180) <= 1e-9 && math.Abs(math.Abs(b)-180) <= 1e-9
}

func multiEqual(a, b orb.MultiLineString) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if !pointsClose(a[i][j], b[i][j]) {
				return false
			}
		}
	}
	return true
}
