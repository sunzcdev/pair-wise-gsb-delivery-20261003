package geohash

import (
	"math"
	"math/rand"
	"testing"
)

// meanEarthRadius is the sphere radius used by the covering tests, in
// meters.
const meanEarthRadius = 6371000.0

// destinationPoint returns the point at angular distance alpha (radians)
// from (lat, lng) along the bearing theta (radians, clockwise from north)
// on a sphere.
func destinationPoint(lat, lng, alpha, theta float64) (float64, float64) {
	rad := math.Pi / 180
	lat1 := lat * rad
	sinLat1, cosLat1 := math.Sin(lat1), math.Cos(lat1)
	sinAlpha, cosAlpha := math.Sin(alpha), math.Cos(alpha)
	lat2 := math.Asin(sinLat1*cosAlpha + cosLat1*sinAlpha*math.Cos(theta))
	lng2 := lng*rad + math.Atan2(
		math.Sin(theta)*sinAlpha*cosLat1,
		cosAlpha-sinLat1*math.Sin(lat2),
	)
	return lat2 / rad, wrapLng(lng2 / rad)
}

// assertCapCovered verifies that the given geohash prefixes conservatively
// cover the spherical cap of angular radius alpha around (lat, lng): every
// sampled boundary and interior point of the cap must lie in the bounding
// box of at least one prefix.
func assertCapCovered(t *testing.T, hashes []string, lat, lng, alpha float64) {
	t.Helper()
	boxes := make([]Box, len(hashes))
	for i, h := range hashes {
		boxes[i] = BoundingBox(h)
	}
	covered := func(plat, plng float64) bool {
		for _, b := range boxes {
			if b.Contains(plat, plng) {
				return true
			}
		}
		return false
	}
	// Boundary points at one-degree bearing intervals.
	for deg := 0; deg < 360; deg++ {
		plat, plng := destinationPoint(lat, lng, alpha, float64(deg)*math.Pi/180)
		if !covered(plat, plng) {
			t.Fatalf("boundary point (%v, %v) at bearing %d not covered by %v", plat, plng, deg, hashes)
		}
	}
	// Random interior points.
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 1000; i++ {
		d := alpha * math.Sqrt(rng.Float64())
		theta := 2 * math.Pi * rng.Float64()
		plat, plng := destinationPoint(lat, lng, d, theta)
		if !covered(plat, plng) {
			t.Fatalf("interior point (%v, %v) not covered by %v", plat, plng, hashes)
		}
	}
}

func TestCoveringEquator(t *testing.T) {
	// 10km circle on the equator at the prime meridian.
	hashes, err := Covering(0, 0, 10000, meanEarthRadius, CoveringConfig{MaxChars: 5})
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{
		"7zzzr", "7zzzv", "7zzzw", "7zzzx", "7zzzy", "7zzzz",
		"ebpbj", "ebpbn", "ebpbp", "ebpbq", "ebpbr", "ebpbx",
		"kpbp2", "kpbp8", "kpbp9", "kpbpb", "kpbpc", "kpbpf",
		"s0000", "s0001", "s0002", "s0003", "s0004", "s0008",
	}
	if !equalStrings(hashes, expected) {
		t.Fatalf("got %v want %v", hashes, expected)
	}
	assertCapCovered(t, hashes, 0, 0, 10000/meanEarthRadius)
}

// TestCoveringEquatorBruteForce cross-checks Covering against an
// independent enumeration of all cells in the cap's lat/lng bounding
// range.
func TestCoveringEquatorBruteForce(t *testing.T) {
	lat, lng, radius := 0.0, 0.0, 10000.0
	alpha := radius / meanEarthRadius
	got, err := Covering(lat, lng, radius, meanEarthRadius, CoveringConfig{MaxChars: 5})
	if err != nil {
		t.Fatal(err)
	}
	// At 5 characters a cell is 180/2^12 degrees in latitude and
	// 360/2^13 degrees in longitude. Enumerate all cells in a bounding
	// range around the cap and filter with the exact intersection test.
	deg := 180 / math.Pi
	margin := alpha*deg + 0.2
	var want []string
	for ilat := 0; ilat < 4096; ilat++ {
		clat := -90 + 180*(float64(ilat)+0.5)/4096
		if math.Abs(clat-lat) > margin {
			continue
		}
		for ilng := 0; ilng < 8192; ilng++ {
			clng := -180 + 360*(float64(ilng)+0.5)/8192
			if math.Abs(wrapLng(clng-lng)) > margin {
				continue
			}
			h := EncodeWithPrecision(clat, clng, 5)
			if circleMayIntersectBox(lat, lng, alpha, BoundingBox(h)) {
				want = append(want, h)
			}
		}
	}
	sortStrings(want)
	if !equalStrings(got, want) {
		t.Fatalf("mismatch\ngot  %v\nwant %v", got, want)
	}
}

func TestCoveringAntimeridian(t *testing.T) {
	// 20km circle crossing the antimeridian.
	hashes, err := Covering(10, 179.95, 20000, meanEarthRadius, CoveringConfig{MaxChars: 5})
	if err != nil {
		t.Fatal(err)
	}
	// The covering must include cells on both sides of the antimeridian.
	east, west := false, false
	for _, h := range hashes {
		b := BoundingBox(h)
		if b.MaxLng > 179.9 {
			east = true
		}
		if b.MinLng < -179.9 {
			west = true
		}
	}
	if !east || !west {
		t.Fatalf("covering %v does not span the antimeridian", hashes)
	}
	assertCapCovered(t, hashes, 10, 179.95, 20000/meanEarthRadius)
}

func TestCoveringNorthPole(t *testing.T) {
	// 700km circle around (85, 30) encloses the north pole, so the
	// covering spans the full range of longitudes.
	hashes, err := Covering(85, 30, 700000, meanEarthRadius, CoveringConfig{MaxChars: 4})
	if err != nil {
		t.Fatal(err)
	}
	// Points near the pole at any longitude must be covered.
	boxes := make([]Box, len(hashes))
	for i, h := range hashes {
		boxes[i] = BoundingBox(h)
	}
	for lng := -180.0; lng < 180; lng += 5 {
		ok := false
		for _, b := range boxes {
			if b.Contains(89, lng) {
				ok = true
				break
			}
		}
		if !ok {
			t.Fatalf("point (89, %v) not covered by %v", lng, hashes)
		}
	}
	assertCapCovered(t, hashes, 85, 30, 700000/meanEarthRadius)
}

func TestCoveringBudgetCoarsening(t *testing.T) {
	// The same 50km circle at increasing cell budgets. A tight budget
	// forces a coarser uniform precision.
	fine, err := Covering(0, 0, 50000, meanEarthRadius, CoveringConfig{MaxChars: 6})
	if err != nil {
		t.Fatal(err)
	}
	coarse, err := Covering(0, 0, 50000, meanEarthRadius, CoveringConfig{MaxChars: 6, MaxCells: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(coarse) > 8 {
		t.Fatalf("budget exceeded: got %d cells", len(coarse))
	}
	if len(coarse) >= len(fine) {
		t.Fatalf("expected coarsening: budget result has %d cells, unlimited has %d", len(coarse), len(fine))
	}
	for _, h := range coarse {
		if uint(len(h)) >= 6 {
			t.Fatalf("expected coarser precision than 6, got %q", h)
		}
	}
	// The coarsened result must still conservatively cover the cap.
	assertCapCovered(t, coarse, 0, 0, 50000/meanEarthRadius)
}

func TestCoveringBudgetExtreme(t *testing.T) {
	// A budget of one degenerates to the empty prefix, matching the whole
	// sphere.
	hashes, err := Covering(0, 0, 50000, meanEarthRadius, CoveringConfig{MaxChars: 6, MaxCells: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(hashes, []string{""}) {
		t.Fatalf("got %v want [\"\"]", hashes)
	}
}

func TestCoveringRadiusTooLarge(t *testing.T) {
	// Radii beyond half the sphere circumference are rejected.
	if _, err := Covering(0, 0, math.Pi*meanEarthRadius+1, meanEarthRadius, CoveringConfig{}); err == nil {
		t.Fatal("expected error for radius beyond half the circumference")
	}
	// Exactly half the circumference covers the whole sphere.
	hashes, err := Covering(0, 0, math.Pi*meanEarthRadius, meanEarthRadius, CoveringConfig{MaxChars: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 32*32 {
		t.Fatalf("expected full-sphere covering of 1024 cells, got %d", len(hashes))
	}
}

func TestCoveringInvalid(t *testing.T) {
	cases := []CoveringConfig{
		{MaxChars: 13},
		{MaxCells: -1},
	}
	for _, cfg := range cases {
		if _, err := Covering(0, 0, 1000, meanEarthRadius, cfg); err == nil {
			t.Fatalf("expected error for config %+v", cfg)
		}
	}
	if _, err := Covering(91, 0, 1000, meanEarthRadius, CoveringConfig{}); err == nil {
		t.Fatal("expected error for invalid latitude")
	}
	if _, err := Covering(0, 0, 1000, 0, CoveringConfig{}); err == nil {
		t.Fatal("expected error for invalid sphere radius")
	}
	if _, err := Covering(0, 0, -1, meanEarthRadius, CoveringConfig{}); err == nil {
		t.Fatal("expected error for negative radius")
	}
}

func TestMinAngularDistanceToBox(t *testing.T) {
	deg := math.Pi / 180
	cases := []struct {
		lat, lng float64
		box      Box
		expected float64 // radians
	}{
		// Point inside the box.
		{0, 0, Box{MinLat: -1, MaxLat: 1, MinLng: -1, MaxLng: 1}, 0},
		// Point due north: pure meridian distance.
		{5, 0, Box{MinLat: -1, MaxLat: 1, MinLng: -1, MaxLng: 1}, 4 * deg},
		// Longitude distance at the equator.
		{0, 5, Box{MinLat: -1, MaxLat: 1, MinLng: -1, MaxLng: 1}, 4 * deg},
		// Across the antimeridian.
		{0, -179, Box{MinLat: -1, MaxLat: 1, MinLng: 178, MaxLng: 179}, 2 * deg},
		// Box containing the north pole. The closest point is on the
		// nearest longitude edge, not the pole itself.
		{80, 45, Box{MinLat: 85, MaxLat: 90, MinLng: 100, MaxLng: 110},
			math.Acos(math.Sin(80*deg)*math.Sin(85*deg) + math.Cos(80*deg)*math.Cos(85*deg)*math.Cos(55*deg))},
	}
	for _, c := range cases {
		got := minAngularDistanceToBox(c.lat, c.lng, c.box)
		if math.Abs(got-c.expected) > 1e-9 {
			t.Errorf("minAngularDistanceToBox(%v, %v, %v) = %v, want %v",
				c.lat, c.lng, c.box, got, c.expected)
		}
	}
}

func TestCoveringRandomConservative(t *testing.T) {
	// Random queries must always conservatively cover the cap.
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 50; i++ {
		lat := -80 + 160*rng.Float64()
		lng := -180 + 360*rng.Float64()
		radius := math.Exp2(7 + 10*rng.Float64()) // 128m to ~131km
		maxChars := uint(1 + rng.Intn(6))
		hashes, err := Covering(lat, lng, radius, meanEarthRadius, CoveringConfig{MaxChars: maxChars})
		if err != nil {
			t.Fatal(err)
		}
		assertCapCovered(t, hashes, lat, lng, radius/meanEarthRadius)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
