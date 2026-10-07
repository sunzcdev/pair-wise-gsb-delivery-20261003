package geo

import (
	"math"
	"testing"

	"github.com/paulmach/orb"
)

func TestSplitLineStringAntimeridian(t *testing.T) {
	cases := []struct {
		name     string
		input    orb.LineString
		expected orb.MultiLineString
	}{
		{
			name:     "no crossing",
			input:    orb.LineString{{10, 1}, {20, 2}, {30, 3}},
			expected: orb.MultiLineString{{{10, 1}, {20, 2}, {30, 3}}},
		},
		{
			name:  "simple eastward crossing",
			input: orb.LineString{{179, 0}, {-179, 0}},
			expected: orb.MultiLineString{
				{{179, 0}, {180, 0}},
				{{-180, 0}, {-179, 0}},
			},
		},
		{
			name:  "simple westward crossing",
			input: orb.LineString{{-179, 0}, {179, 0}},
			expected: orb.MultiLineString{
				{{-179, 0}, {-180, 0}},
				{{180, 0}, {179, 0}},
			},
		},
		{
			name:  "latitude linearly interpolated",
			input: orb.LineString{{170, 0}, {-170, 20}},
			expected: orb.MultiLineString{
				{{170, 0}, {180, 10}},
				{{-180, 10}, {-170, 20}},
			},
		},
		{
			name:  "multiple consecutive crossings",
			input: orb.LineString{{170, 0}, {-170, 0}, {170, 0}, {-170, 0}},
			expected: orb.MultiLineString{
				{{170, 0}, {180, 0}},
				{{-180, 0}, {-170, 0}, {-180, 0}},
				{{180, 0}, {170, 0}, {180, 0}},
				{{-180, 0}, {-170, 0}},
			},
		},
		{
			name:     "vertex exactly on the seam is kept, no cut",
			input:    orb.LineString{{170, 5}, {180, 10}, {-170, 15}},
			expected: orb.MultiLineString{{{170, 5}, {180, 10}, {-170, 15}}},
		},
		{
			name:     "vertex exactly on -180 is kept, no cut",
			input:    orb.LineString{{-170, 5}, {-180, 10}, {170, 15}},
			expected: orb.MultiLineString{{{-170, 5}, {-180, 10}, {170, 15}}},
		},
		{
			name:  "exactly 180 degrees apart goes east",
			input: orb.LineString{{100, 0}, {-80, 18}},
			expected: orb.MultiLineString{
				{{100, 0}, {180, 8}},
				{{-180, 8}, {-80, 18}},
			},
		},
		{
			name:     "exactly 180 degrees apart, no seam on the way",
			input:    orb.LineString{{-90, 0}, {90, 10}},
			expected: orb.MultiLineString{{{-90, 0}, {90, 10}}},
		},
		{
			name:     "crossing right after seam vertex",
			input:    orb.LineString{{180, 0}, {-170, 10}},
			expected: orb.MultiLineString{{{180, 0}, {-170, 10}}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := tc.input.Clone()
			result := SplitLineStringAntimeridian(tc.input)

			if !result.Equal(tc.expected) {
				t.Errorf("incorrect result")
				t.Logf("got:  %v", result)
				t.Logf("want: %v", tc.expected)
			}

			if !tc.input.Equal(original) {
				t.Errorf("input was modified: %v", tc.input)
			}
		})
	}
}

func TestSplitLineStringAntimeridian_Degenerate(t *testing.T) {
	if result := SplitLineStringAntimeridian(nil); result != nil {
		t.Errorf("expected nil for empty input, got %v", result)
	}

	one := orb.LineString{{100, 20}}
	result := SplitLineStringAntimeridian(one)
	if len(result) != 1 || !result[0].Equal(one) {
		t.Errorf("expected single point line back, got %v", result)
	}
}

// unfoldLineString maps a line string into a continuous longitude domain
// using the same shortest-direction rule as the splitter.
func unfoldLineString(ls orb.LineString) []orb.Point {
	pts := make([]orb.Point, 0, len(ls))
	unwrapped := ls[0][0]
	pts = append(pts, orb.Point{unwrapped, ls[0][1]})
	for i := 1; i < len(ls); i++ {
		unwrapped += wrapLongitudeDelta(ls[i][0] - ls[i-1][0])
		pts = append(pts, orb.Point{unwrapped, ls[i][1]})
	}
	return pts
}

// unfoldMultiLineString stitches the split pieces back into a continuous
// longitude domain. Junction points (180/-180 pairs) collapse into one.
func unfoldMultiLineString(t *testing.T, mls orb.MultiLineString) []orb.Point {
	t.Helper()

	var pts []orb.Point
	var rawPrev, unwrappedPrev float64
	for i, ls := range mls {
		for j, p := range ls {
			switch {
			case i == 0 && j == 0:
				rawPrev, unwrappedPrev = p[0], p[0]
				pts = append(pts, p)
			case j == 0:
				d := math.Abs(math.Mod(p[0]-rawPrev, 360))
				if d > 1e-9 && 360-d > 1e-9 {
					t.Fatalf("junction not on same meridian: %v -> %v", rawPrev, p[0])
				}
				if math.Abs(p[1]-pts[len(pts)-1][1]) > 1e-9 {
					t.Fatalf("junction latitude mismatch: %v -> %v", pts[len(pts)-1], p)
				}
				rawPrev = p[0] // same physical point, skip
			default:
				unwrappedPrev += wrapLongitudeDelta(p[0] - rawPrev)
				rawPrev = p[0]
				pts = append(pts, orb.Point{unwrappedPrev, p[1]})
			}
		}
	}
	return pts
}

func linearLength(pts []orb.Point) float64 {
	var total float64
	for i := 1; i < len(pts); i++ {
		total += math.Hypot(pts[i][0]-pts[i-1][0], pts[i][1]-pts[i-1][1])
	}
	return total
}

func TestSplitLineStringAntimeridian_Unfold(t *testing.T) {
	// tricky track: multiple crossings, a vertex on the seam and a
	// pair exactly 180 degrees apart.
	input := orb.LineString{
		{170, 0},
		{-170, 10}, // crossing 1, eastward
		{170, 20},  // crossing 2, westward
		{180, 30},  // vertex exactly on the seam, no cut
		{-170, 40}, // shortest step from the seam, still no cut
		{-160, 50},
		{160, 60},  // crossing 3, westward
		{-150, 70}, // crossing 4, eastward
		{100, 80},  // crossing 5, westward
		{-80, 90},  // exactly 180 degrees apart, tie goes east: crossing 6
	}
	original := input.Clone()

	result := SplitLineStringAntimeridian(input)

	if len(result) != 7 {
		t.Fatalf("expected 7 pieces, got %d: %v", len(result), result)
	}
	for i, ls := range result {
		if len(ls) < 2 {
			t.Fatalf("piece %d has fewer than 2 points: %v", i, ls)
		}
	}

	want := unfoldLineString(input)
	got := unfoldMultiLineString(t, result)

	if l1, l2 := linearLength(got), linearLength(want); math.Abs(l1-l2) > 1e-9 {
		t.Errorf("total linear length mismatch: got %v, want %v", l1, l2)
	}

	// the original vertices must appear in order in the split+unfolded
	// track, i.e. splitting only inserts seam intersection points.
	next := 0
	for _, p := range got {
		if next < len(want) &&
			math.Abs(p[0]-want[next][0]) < 1e-9 &&
			math.Abs(p[1]-want[next][1]) < 1e-9 {
			next++
		}
	}
	if next != len(want) {
		t.Errorf("original track order not preserved")
		t.Logf("unfolded original: %v", want)
		t.Logf("unfolded split:    %v", got)
	}

	if !input.Equal(original) {
		t.Errorf("input was modified: %v", input)
	}
}
