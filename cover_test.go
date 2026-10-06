package geohash

import (
	"math"
	"testing"
)

// testSphereRadius is the spherical radius used throughout these tests, in
// meters. The cover algorithm must work for any caller-supplied value.
const testSphereRadius = 6371e3

func radians(deg float64) float64 { return deg * math.Pi / 180 }

// destinationPoint returns the point at angular distance theta (radians) from
// (lat, lng) along the given bearing (radians, clockwise from north).
func destinationPoint(lat, lng, theta, bearing float64) (float64, float64) {
	phi1 := radians(lat)
	lambda1 := radians(lng)
	sinPhi1, cosPhi1 := math.Sincos(phi1)
	sinTheta, cosTheta := math.Sincos(theta)
	sinB, cosB := math.Sincos(bearing)
	sinPhi2 := sinPhi1*cosTheta + cosPhi1*sinTheta*cosB
	phi2 := math.Asin(sinPhi2)
	lambda2 := lambda1 + math.Atan2(sinB*sinTheta*cosPhi1, cosTheta-sinPhi1*math.Sin(phi2))
	return phi2 * 180 / math.Pi, lambda2 * 180 / math.Pi
}

func normalizeLng(lng float64) float64 {
	return math.Mod(lng+540, 360) - 180
}

// coveredBy reports whether the point falls inside any cell of the cover.
func coveredBy(cover []string, lat, lng float64) bool {
	lng = normalizeLng(lng)
	for _, hash := range cover {
		if hash == "" {
			return true
		}
		if BoundingBox(hash).Contains(lat, lng) {
			return true
		}
	}
	return false
}

// assertCapCovered fails the test if any sampled point of the cap (boundary
// ring plus interior rings) is missing from the cover.
func assertCapCovered(t *testing.T, cover []string, lat, lng, theta float64) {
	t.Helper()
	for _, fraction := range []float64{1.0, 0.75, 0.5, 0.25, 0.0} {
		steps := 720
		if fraction == 0 {
			steps = 1
		}
		for i := 0; i < steps; i++ {
			bearing := 2 * math.Pi * float64(i) / float64(steps)
			plat, plng := destinationPoint(lat, lng, theta*fraction, bearing)
			if !coveredBy(cover, plat, plng) {
				t.Fatalf("cap point (%v, %v) at fraction %v bearing %v not covered",
					plat, plng, fraction, bearing)
			}
		}
	}
}

// assertNoProvablyDisjointCells fails the test if the cover contains a cell
// whose exact minimum distance to the center exceeds the cap radius.
func assertNoProvablyDisjointCells(t *testing.T, cover []string, lat, lng, theta float64) {
	t.Helper()
	limit := theta*(1+1e-9) + 1e-12
	for _, hash := range cover {
		if hash == "" {
			continue
		}
		if d := minAngularDistanceToBox(lat, lng, BoundingBox(hash)); d > limit {
			t.Fatalf("cell %q is provably disjoint: min distance %v > %v", hash, d, theta)
		}
	}
}

func TestMinAngularDistanceToBox(t *testing.T) {
	deg := math.Pi / 180
	cases := []struct {
		name string
		lat  float64
		lng  float64
		box  Box
		want float64 // radians
	}{
		{"interior", 10, 20, Box{MinLat: 0, MaxLat: 20, MinLng: 10, MaxLng: 30}, 0},
		{"edge", 0, 20, Box{MinLat: 0, MaxLat: 20, MinLng: 10, MaxLng: 30}, 0},
		{"north", 0, 0, Box{MinLat: 1, MaxLat: 2, MinLng: -1, MaxLng: 1}, deg},
		{"east", 0, 0, Box{MinLat: -1, MaxLat: 1, MinLng: 1, MaxLng: 2}, deg},
		{"corner", 0, 0, Box{MinLat: 1, MaxLat: 2, MinLng: 1, MaxLng: 2}, angularDistance(0, 0, 1, 1)},
		{"wrap", 0, -179.5, Box{MinLat: -1, MaxLat: 1, MinLng: 179, MaxLng: 180}, 0.5 * deg},
		{"pole", 90, 0, Box{MinLat: 80, MaxLat: 85, MinLng: 100, MaxLng: 110}, 5 * deg},
	}
	for _, c := range cases {
		got := minAngularDistanceToBox(c.lat, c.lng, c.box)
		if math.Abs(got-c.want) > 1e-12 {
			t.Errorf("%s: minAngularDistanceToBox = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCoverCircleEquator(t *testing.T) {
	lat, lng := 0.0, 0.0
	radius := 20e3 // 20 km
	theta := radius / testSphereRadius
	cover := CoverCircle(lat, lng, radius, testSphereRadius, 6, 0)
	if len(cover) == 0 {
		t.Fatal("empty cover")
	}
	for _, hash := range cover {
		if len(hash) != 6 {
			t.Fatalf("hash %q has length %d, want 6", hash, len(hash))
		}
	}
	assertCapCovered(t, cover, lat, lng, theta)
	assertNoProvablyDisjointCells(t, cover, lat, lng, theta)

	// The cover must not be decided by cell-center distance: boundary
	// cells whose centers lie outside the cap must still be retained.
	found := false
	for _, hash := range cover {
		clat, clng := DecodeCenter(hash)
		if angularDistance(lat, lng, clat, clng) > theta {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no retained cell has its center outside the cap; " +
			"cover appears to be decided by cell-center distance")
	}
}

func TestCoverCircleAntimeridian(t *testing.T) {
	lat, lng := 0.0, 179.9
	radius := 30e3 // 30 km, crosses the antimeridian
	theta := radius / testSphereRadius
	cover := CoverCircle(lat, lng, radius, testSphereRadius, 6, 0)
	assertCapCovered(t, cover, lat, lng, theta)
	assertNoProvablyDisjointCells(t, cover, lat, lng, theta)

	// The cover must include cells on both sides of the antimeridian.
	east, west := false, false
	for _, hash := range cover {
		box := BoundingBox(hash)
		if box.MinLng >= 179 {
			east = true
		}
		if box.MaxLng <= -179 {
			west = true
		}
	}
	if !east || !west {
		t.Fatalf("cover does not span the antimeridian: east=%v west=%v", east, west)
	}
}

func TestCoverCircleNorthPole(t *testing.T) {
	lat, lng := 85.0, 30.0
	theta := radians(10) // cap encloses the north pole
	radius := theta * testSphereRadius
	cover := CoverCircle(lat, lng, radius, testSphereRadius, 3, 0)
	assertCapCovered(t, cover, lat, lng, theta)
	assertNoProvablyDisjointCells(t, cover, lat, lng, theta)

	// The pole itself must be covered.
	if !coveredBy(cover, 90, 0) {
		t.Fatal("north pole not covered")
	}
	// At latitude 88 every longitude is within 10 degrees of the center
	// (worst case is about 7 degrees), so the full circle must be covered.
	for lng := -180.0; lng < 180; lng += 5 {
		if !coveredBy(cover, 88, lng) {
			t.Fatalf("point (88, %v) inside polar cap not covered", lng)
		}
	}
}

// TestCoverCircleBoundaryPoints is a hand-checkable case: a one-degree cap on
// the equator. The four cardinal boundary points are exactly one degree from
// the center, so their own geohash cells must appear in the cover.
func TestCoverCircleBoundaryPoints(t *testing.T) {
	lat, lng := 0.0, 0.0
	theta := radians(1)
	radius := theta * testSphereRadius
	const chars = 4
	cover := CoverCircle(lat, lng, radius, testSphereRadius, chars, 0)

	points := [][2]float64{{1, 0}, {-1, 0}, {0, 1}, {0, -1}}
	for _, p := range points {
		want := EncodeWithPrecision(p[0], p[1], chars)
		found := false
		for _, hash := range cover {
			if hash == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("boundary point %v: cell %q missing from cover", p, want)
		}
	}

	// Diagonal boundary points at exactly the cap radius must be covered.
	for _, bearingDeg := range []float64{45, 135, 225, 315} {
		plat, plng := destinationPoint(lat, lng, theta, radians(bearingDeg))
		if !coveredBy(cover, plat, plng) {
			t.Errorf("boundary point at bearing %v not covered", bearingDeg)
		}
	}
}

func TestCoverCircleZeroRadius(t *testing.T) {
	lat, lng := 12.34, 56.78
	cover := CoverCircle(lat, lng, 0, testSphereRadius, 5, 0)
	want := EncodeWithPrecision(lat, lng, 5)
	if len(cover) != 1 || cover[0] != want {
		t.Fatalf("zero radius: got %v, want [%q]", cover, want)
	}
}

func TestCoverCircleBudget(t *testing.T) {
	lat, lng := 0.0, 0.0
	radius := 500e3 // 500 km, about 4.5 degrees
	theta := radius / testSphereRadius

	// With a budget of 10 cells the precision-3 cover (dozens of cells) is
	// unaffordable, so the result must coarsen to the four precision-2 cells
	// around the origin.
	cover := CoverCircle(lat, lng, radius, testSphereRadius, 6, 10)
	if len(cover) != 4 {
		t.Fatalf("budgeted cover has %d cells, want 4", len(cover))
	}
	for _, hash := range cover {
		if len(hash) != 2 {
			t.Fatalf("budgeted hash %q has length %d, want 2", hash, len(hash))
		}
	}
	// Coarsening must remain conservative.
	assertCapCovered(t, cover, lat, lng, theta)

	// A budget below the coarsest non-trivial cover degrades to the whole
	// sphere, represented by the empty prefix.
	cover = CoverCircle(lat, lng, radius, testSphereRadius, 6, 1)
	if len(cover) != 1 || cover[0] != "" {
		t.Fatalf("budget 1: got %v, want [\"\"]", cover)
	}
}

func TestCoverCircleWholeSphere(t *testing.T) {
	// Half the sphere circumference or more covers everything.
	for _, radius := range []float64{
		math.Pi * testSphereRadius,
		2 * math.Pi * testSphereRadius,
	} {
		cover := CoverCircle(0, 0, radius, testSphereRadius, 6, 0)
		if len(cover) != 1 || cover[0] != "" {
			t.Fatalf("radius %v: got %v, want [\"\"]", radius, cover)
		}
	}

	// Just under half the circumference must still produce a real cover.
	radius := 0.9 * math.Pi * testSphereRadius
	cover := CoverCircle(0, 0, radius, testSphereRadius, 2, 0)
	if len(cover) == 0 || (len(cover) == 1 && cover[0] == "") {
		t.Fatalf("radius %v: unexpected whole-sphere cover %v", radius, cover)
	}
	assertCapCovered(t, cover, 0, 0, 0.9*math.Pi)
}
