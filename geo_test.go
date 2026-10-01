package buntdb

import (
	"errors"
	"math"
	"math/rand"
	"sort"
	"testing"
)

// geoRefPoint is a point used by the brute-force reference implementation.
type geoRefPoint struct {
	key  string
	lon  float64
	lat  float64
	dist float64
}

// refDistance is an independent great-circle implementation based on the
// spherical law of cosines. The index uses haversine/chords, so agreement
// between the two gives a meaningful cross-check.
func refDistance(lon1, lat1, lon2, lat2, radius float64) float64 {
	r := math.Pi / 180
	la1, la2 := lat1*r, lat2*r
	dlon := (lon2 - lon1)
	if dlon > 180 {
		dlon -= 360
	} else if dlon < -180 {
		dlon += 360
	}
	dlon *= r
	cos := math.Sin(la1)*math.Sin(la2) +
		math.Cos(la1)*math.Cos(la2)*math.Cos(dlon)
	if cos > 1 {
		cos = 1
	} else if cos < -1 {
		cos = -1
	}
	return radius * math.Acos(cos)
}

func refTopK(points []geoRefPoint, lon, lat float64, k int, radius float64) []geoRefPoint {
	for i := range points {
		points[i].dist = refDistance(lon, lat, points[i].lon, points[i].lat, radius)
	}
	sort.SliceStable(points, func(i, j int) bool {
		eq := func(a, b float64) bool {
			return math.Abs(a-b) <= math.Max(1e-9, math.Max(math.Abs(a), math.Abs(b))*1e-10)
		}
		if !eq(points[i].dist, points[j].dist) {
			return points[i].dist < points[j].dist
		}
		return points[i].key < points[j].key
	})
	if k < len(points) {
		points = points[:k]
	}
	return points
}

func geoPointsFromDB(t *testing.T, db *DB, indexName string) []geoRefPoint {
	t.Helper()
	var pts []geoRefPoint
	err := db.View(func(tx *Tx) error {
		return tx.Ascend(indexName, func(key, value string) bool {
			min, max := IndexRect(value)
			if len(min) == 2 && min[0] == max[0] && min[1] == max[1] {
				pts = append(pts, geoRefPoint{key: key, lon: min[0], lat: min[1]})
			}
			return true
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return pts
}

type geoHit struct {
	key  string
	dist float64
}

func runGeoQuery(t *testing.T, db *DB, index string, lon, lat float64, k int,
	unit GeoUnit) ([]geoHit, error) {
	t.Helper()
	var hits []geoHit
	err := db.View(func(tx *Tx) error {
		return tx.NearbyGeo(index, lon, lat, k, unit,
			func(key, value string, dist float64) bool {
				hits = append(hits, geoHit{key: key, dist: dist})
				return true
			})
	})
	return hits, err
}

func assertGeoHits(t *testing.T, hits []geoHit, want []geoRefPoint, radius float64) {
	t.Helper()
	if len(hits) != len(want) {
		t.Fatalf("got %d hits, want %d", len(hits), len(want))
	}
	tol := math.Max(1e-9, radius*1e-12)
	for i, w := range want {
		if math.Abs(hits[i].dist-w.dist) > math.Max(tol, math.Abs(w.dist)*1e-10) {
			t.Fatalf("hit %d (%s): got dist %.9f, want %.9f",
				i, w.key, hits[i].dist, w.dist)
		}
		if i > 0 {
			if hits[i].dist < hits[i-1].dist-tol {
				t.Fatalf("hits not distance-ordered at %d", i)
			}
			if math.Abs(hits[i].dist-hits[i-1].dist) <= 1e-9 &&
				hits[i].key < hits[i-1].key {
				t.Fatalf("equal-distance hits not key-ordered at %d", i)
			}
		}
	}
	// Match keys. When the independent reference formula produces a
	// near-tie band (differences within numerical noise), the index may
	// order those entries in any internally consistent way; verify each
	// returned key belongs to the reference band at that distance.
	eqTol := radius * 1e-9
	for i, h := range hits {
		if h.key == want[i].key {
			continue
		}
		matched := false
		for j, w := range want {
			if w.key == h.key && math.Abs(w.dist-h.dist) <= math.Max(eqTol, 1e-6) {
				// Allow swaps only among entries that are mutually
				// near-tied in both orderings.
				if math.Abs(want[j].dist-want[i].dist) <= math.Max(eqTol, 1e-6) {
					matched = true
					break
				}
			}
		}
		if !matched {
			t.Fatalf("hit %d: got key %s, want %s (distances %.9f vs %.9f)",
				i, h.key, want[i].key, h.dist, want[i].dist)
		}
	}
}

// TestNearbyGeoAntimeridian places points on both sides of the 180th
// meridian and queries from both the +180 and -180 representations. A planar
// distance would report points as being 360 degrees apart here.
func TestNearbyGeoAntimeridian(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateGeoIndex("geo", "*", IndexRect); err != nil {
		t.Fatal(err)
	}

	pts := []geoRefPoint{
		{key: "a:179.9E", lon: 179.9, lat: 0},
		{key: "b:179.0E", lon: 179, lat: 0},
		{key: "c:180.0", lon: 180, lat: 0},
		{key: "d:179.9W", lon: -179.9, lat: 0},
		{key: "e:179.0W", lon: -179, lat: 0},
		{key: "f:0", lon: 0, lat: 0},
	}
	if err := db.Update(func(tx *Tx) error {
		for _, p := range pts {
			if _, _, err := tx.Set(p.key, Point(p.lon, p.lat), nil); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	radius := earthRadiusMeters
	want := refTopK(append([]geoRefPoint(nil), pts...), 180, 0, 4, radius)

	// Querying from +180 ...
	hits, err := runGeoQuery(t, db, "geo", 180, 0, 4, GeoMeters)
	if err != nil {
		t.Fatal(err)
	}
	assertGeoHits(t, hits, want, radius)

	// ... and from -180 must produce the same result, since they are the
	// same meridian.
	hits2, err := runGeoQuery(t, db, "geo", -180, 0, 4, GeoMeters)
	if err != nil {
		t.Fatal(err)
	}
	assertGeoHits(t, hits2, want, radius)

	// Expected concrete ordering: the two 179.9 points (0.1 degree away) tie
	// and resolve by key, then the 180.0 point (distance zero actually comes
	// first). Sanity-check that directly.
	if want[0].key != "c:180.0" || want[0].dist != 0 {
		t.Fatalf("nearest to the seam should be c:180.0, got %s", want[0].key)
	}
	if want[1].key != "a:179.9E" || want[2].key != "d:179.9W" {
		t.Fatalf("seam tie ordering wrong: %s, %s", want[1].key, want[2].key)
	}

	// A query just off the seam on the + side finds the -179.9 point closer
	// than the +179 point, which is impossible with planar distance.
	hits3, err := runGeoQuery(t, db, "geo", 179.95, 0, 3, GeoMeters)
	if err != nil {
		t.Fatal(err)
	}
	if hits3[0].key != "a:179.9E" ||
		(hits3[1].key != "d:179.9W" && hits3[1].key != "c:180.0") {
		t.Fatalf("unexpected cross-seam ordering: %v", hits3)
	}
}

// TestNearbyGeoPoles checks ordering near the north and south poles, where
// longitude is meaningless and planar distance is most distorted.
func TestNearbyGeoPoles(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateGeoIndex("geo", "*", IndexRect); err != nil {
		t.Fatal(err)
	}

	// Points at the same latitude but wildly different longitudes must be
	// equidistant from the pole.
	pts := []geoRefPoint{
		{key: "n0", lon: 0, lat: 89},
		{key: "n1", lon: 90, lat: 89},
		{key: "n2", lon: -90, lat: 89},
		{key: "n3", lon: 180, lat: 89},
		{key: "n4", lon: -180, lat: 89},
		{key: "near", lon: 45, lat: 89.9},
		{key: "equator", lon: 45, lat: 0},
	}
	if err := db.Update(func(tx *Tx) error {
		for _, p := range pts {
			if _, _, err := tx.Set(p.key, Point(p.lon, p.lat), nil); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	radius := earthRadiusKilometers
	want := refTopK(append([]geoRefPoint(nil), pts...), 12, 89.999, 6, radius)
	hits, err := runGeoQuery(t, db, "geo", 12, 89.999, 6, GeoKilometers)
	if err != nil {
		t.Fatal(err)
	}
	assertGeoHits(t, hits, want, radius)

	// The five 89-degree points are within ~1m of each other regardless of
	// longitude; spherical formulas differ by sub-meter numerical noise.
	tied := map[string]bool{"n0": true, "n1": true, "n2": true, "n3": true, "n4": true}
	for i := 1; i <= 5; i++ {
		if !tied[hits[i].key] {
			t.Fatalf("expected pole ties in slots 1..5, got %s at %d", hits[i].key, i)
		}
		if math.Abs(hits[i].dist-hits[1].dist) > 1.0 {
			t.Fatalf("pole points differ by more than 1m: %f vs %f", hits[i].dist, hits[1].dist)
		}
	}

	// South pole symmetric check.
	south := []geoRefPoint{
		{key: "s0", lon: 10, lat: -88},
		{key: "s1", lon: -170, lat: -88},
		{key: "s2", lon: 170, lat: -88},
		{key: "far", lon: 0, lat: 10},
	}
	db2, _ := Open(":memory:")
	defer db2.Close()
	db2.CreateGeoIndex("geo", "*", IndexRect)
	db2.Update(func(tx *Tx) error {
		for _, p := range south {
			tx.Set(p.key, Point(p.lon, p.lat), nil)
		}
		return nil
	})
	swant := refTopK(append([]geoRefPoint(nil), south...), 75, -90, 3, earthRadiusMiles)
	shits, err := runGeoQuery(t, db2, "geo", 75, -90, 3, GeoMiles)
	if err != nil {
		t.Fatal(err)
	}
	assertGeoHits(t, shits, swant, earthRadiusMiles)
}

// TestNearbyGeoRandomBruteForce builds a small global dataset and compares
// top-k results against a full brute-force recomputation for many queries,
// including queries clustered on the seam and at the poles.
func TestNearbyGeoRandomBruteForce(t *testing.T) {
	const N = 400
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateGeoIndex("geo", "*", IndexRect); err != nil {
		t.Fatal(err)
	}

	rng := rand.New(rand.NewSource(42))
	pts := make([]geoRefPoint, 0, N)
	if err := db.Update(func(tx *Tx) error {
		for i := 0; i < N; i++ {
			// Uniform-ish on the sphere in longitude; bias some latitudes
			// toward the poles by mixing in extreme values.
			lon := rng.Float64()*360 - 180
			var lat float64
			switch i % 5 {
			case 0:
				lat = rng.Float64()*4 - 2 // near south pole
			case 1:
				lat = rng.Float64()*4 + 86 // near north pole
			case 2:
				lat = rng.Float64()*4 - 90
			default:
				lat = math.Asin(2*rng.Float64()-1) * 180 / math.Pi
			}
			if lat < -90 {
				lat = -90
			}
			if lat > 90 {
				lat = 90
			}
			key := "p:" + string(rune('a'+i%26)) + ":" + itoa(i)
			pts = append(pts, geoRefPoint{key: key, lon: lon, lat: lat})
			if _, _, err := tx.Set(key, Point(lon, lat), nil); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	type query struct {
		lon, lat float64
		k        int
		unit     GeoUnit
		radius   float64
	}
	queries := []query{
		{180, 0, 10, GeoMeters, earthRadiusMeters},
		{-180, 0, 10, GeoMeters, earthRadiusMeters},
		{179.99, 1, 15, GeoKilometers, earthRadiusKilometers},
		{-179.99, -1, 5, GeoMiles, earthRadiusMiles},
		{0, 90, 20, GeoMeters, earthRadiusMeters},
		{0, -90, 7, GeoKilometers, earthRadiusKilometers},
		{45, 89.999, 30, GeoMiles, earthRadiusMiles},
		{-45, -89.999, 3, GeoMeters, earthRadiusMeters},
		{12.5, -33.8, 50, GeoMeters, earthRadiusMeters},
		{-74.0, 40.7, 1, GeoKilometers, earthRadiusKilometers},
		{139.7, 35.7, 100, GeoMiles, earthRadiusMiles},
	}
	for i := 0; i < 40; i++ {
		lon := rng.Float64()*360 - 180
		lat := math.Asin(2*rng.Float64()-1) * 180 / math.Pi
		queries = append(queries, query{
			lon: lon, lat: lat, k: 1 + rng.Intn(60),
			unit: GeoMeters, radius: earthRadiusMeters,
		})
	}

	for i, qc := range queries {
		want := refTopK(append([]geoRefPoint(nil), pts...),
			qc.lon, qc.lat, qc.k, qc.radius)
		hits, err := runGeoQuery(t, db, "geo", qc.lon, qc.lat, qc.k, qc.unit)
		if err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
		assertGeoHits(t, hits, want, qc.radius)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}

// TestNearbyGeoTieOrder constructs points at the exact same distance and
// checks deterministic key-ascending order.
func TestNearbyGeoTieOrder(t *testing.T) {
	db, _ := Open(":memory:")
	defer db.Close()
	db.CreateGeoIndex("geo", "*", IndexRect)
	// Four points at distance 1 degree from (0,0): east/west/north/south.
	db.Update(func(tx *Tx) error {
		tx.Set("east", Point(1, 0), nil)
		tx.Set("west", Point(-1, 0), nil)
		tx.Set("north", Point(0, 1), nil)
		tx.Set("south", Point(0, -1), nil)
		tx.Set("origin", Point(0, 0), nil)
		return nil
	})
	hits, err := runGeoQuery(t, db, "geo", 0, 0, 10, GeoMeters)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 5 {
		t.Fatalf("want 5 hits, got %d", len(hits))
	}
	if hits[0].key != "origin" || hits[0].dist != 0 {
		t.Fatalf("first should be origin, got %s", hits[0].key)
	}
	wantOrder := []string{"east", "north", "south", "west"}
	for i, w := range wantOrder {
		if hits[i+1].key != w {
			t.Fatalf("tie slot %d: got %s, want %s", i+1, hits[i+1].key, w)
		}
		if i > 0 {
			if d := math.Abs(hits[i+1].dist - hits[i].dist); d > 1e-6 {
				t.Fatalf("tie distances differ: %f", d)
			}
		}
	}
}

// TestNearbyGeoUnits verifies the three units agree up to their constants.
func TestNearbyGeoUnits(t *testing.T) {
	db, _ := Open(":memory:")
	defer db.Close()
	db.CreateGeoIndex("geo", "*", IndexRect)
	db.Update(func(tx *Tx) error {
		tx.Set("p1", Point(10, 10), nil)
		tx.Set("p2", Point(-170, -10), nil)
		return nil
	})
	hm, _ := runGeoQuery(t, db, "geo", 0, 0, 2, GeoMeters)
	hk, _ := runGeoQuery(t, db, "geo", 0, 0, 2, GeoKilometers)
	hmi, _ := runGeoQuery(t, db, "geo", 0, 0, 2, GeoMiles)
	for i := 0; i < 2; i++ {
		if math.Abs(hm[i].dist/1000-hk[i].dist) > 1e-7 {
			t.Fatalf("meter/km mismatch at %d", i)
		}
		if math.Abs(hm[i].dist/1609.344-hmi[i].dist) > 1e-7 {
			t.Fatalf("meter/mile mismatch at %d", i)
		}
	}

	// Standalone GeoDistance agrees with the query results.
	d, err := GeoDistance(0, 0, 10, 10, GeoMeters)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(d-hm[0].dist) > 1e-6 {
		t.Fatalf("GeoDistance %f != query %f", d, hm[0].dist)
	}
	if _, err := GeoDistance(181, 0, 0, 0, GeoMeters); err != ErrInvalidCoordinates {
		t.Fatalf("want ErrInvalidCoordinates, got %v", err)
	}
	if _, err := GeoDistance(0, 91, 0, 0, GeoMeters); err != ErrInvalidCoordinates {
		t.Fatalf("want ErrInvalidCoordinates, got %v", err)
	}
}

// TestNearbyGeoErrors covers argument and index validation.
func TestNearbyGeoErrors(t *testing.T) {
	db, _ := Open(":memory:")
	defer db.Close()
	db.CreateGeoIndex("geo", "*", IndexRect)
	db.CreateSpatialIndex("planar", "*", IndexRect)
	db.CreateIndex("btree", "*")
	db.Update(func(tx *Tx) error {
		tx.Set("p", Point(1, 1), nil)
		return nil
	})

	if err := db.View(func(tx *Tx) error {
		return tx.NearbyGeo("geo", 181, 0, 1, GeoMeters, func(string, string, float64) bool { return true })
	}); err != ErrInvalidCoordinates {
		t.Fatalf("want ErrInvalidCoordinates, got %v", err)
	}
	if err := db.View(func(tx *Tx) error {
		return tx.NearbyGeo("geo", 0, -91, 1, GeoMeters, func(string, string, float64) bool { return true })
	}); err != ErrInvalidCoordinates {
		t.Fatalf("want ErrInvalidCoordinates, got %v", err)
	}
	if err := db.View(func(tx *Tx) error {
		return tx.NearbyGeo("missing", 0, 0, 1, GeoMeters, func(string, string, float64) bool { return true })
	}); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := db.View(func(tx *Tx) error {
		return tx.NearbyGeo("planar", 0, 0, 1, GeoMeters, func(string, string, float64) bool { return true })
	}); err != ErrInvalidOperation {
		t.Fatalf("want ErrInvalidOperation for planar index, got %v", err)
	}
	if err := db.View(func(tx *Tx) error {
		return tx.NearbyGeo("btree", 0, 0, 1, GeoMeters, func(string, string, float64) bool { return true })
	}); err != ErrInvalidOperation {
		t.Fatalf("want ErrInvalidOperation for btree index, got %v", err)
	}
	if err := db.View(func(tx *Tx) error {
		return tx.NearbyGeo("geo", 0, 0, 1, GeoUnit(99), func(string, string, float64) bool { return true })
	}); err != ErrInvalidOperation {
		t.Fatalf("want ErrInvalidOperation for bad unit, got %v", err)
	}

	// k <= 0 returns nothing without error.
	called := false
	err := db.View(func(tx *Tx) error {
		return tx.NearbyGeo("geo", 0, 0, 0, GeoMeters, func(string, string, float64) bool {
			called = true
			return true
		})
	})
	if err != nil || called {
		t.Fatalf("k=0 should be a no-op, err=%v called=%v", err, called)
	}
}

// TestNearbyGeoInvalidItems makes sure values that are not valid geographic
// points are left out of the geo index but do not break it.
func TestNearbyGeoInvalidItems(t *testing.T) {
	db, _ := Open(":memory:")
	defer db.Close()
	if err := db.CreateGeoIndex("geo", "*", GeoIndexRect); err != nil {
		t.Fatal(err)
	}
	db.Update(func(tx *Tx) error {
		tx.Set("good", Point(10, 20), nil)
		tx.Set("bad-lon", Point(200, 20), nil)
		tx.Set("bad-lat", Point(10, 100), nil)
		tx.Set("rect", "[1 1],[2 2]", nil)
		tx.Set("nonsense", "hello", nil)
		return nil
	})
	hits, err := runGeoQuery(t, db, "geo", 0, 0, 10, GeoMeters)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].key != "good" {
		t.Fatalf("only 'good' should be indexed, got %v", hits)
	}

	// The index also maintains an r-tree, but non-point values are not
	// geo-rects either, so they are absent from spatial queries on it too.
	var keys []string
	db.View(func(tx *Tx) error {
		return tx.Intersects("geo", "[1 1],[2 2]", func(key, value string) bool {
			keys = append(keys, key)
			return true
		})
	})
	if len(keys) != 0 {
		t.Fatalf("non-point values must not enter the geo index, got %v", keys)
	}
}

// TestNearbyGeoMutations exercises inserts, updates, deletes and index
// rebuild against the brute-force reference, including many deletes that
// force tombstone rebuilds.
func TestNearbyGeoMutations(t *testing.T) {
	db, _ := Open(":memory:")
	defer db.Close()
	db.CreateGeoIndex("geo", "*", IndexRect)

	rng := rand.New(rand.NewSource(7))
	live := map[string]geoRefPoint{}
	upsert := func(key string, lon, lat float64) {
		if err := db.Update(func(tx *Tx) error {
			_, _, err := tx.Set(key, Point(lon, lat), nil)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		live[key] = geoRefPoint{key: key, lon: lon, lat: lat}
	}
	del := func(key string) {
		if err := db.Update(func(tx *Tx) error {
			_, err := tx.Delete(key)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		delete(live, key)
	}

	for i := 0; i < 300; i++ {
		key := "k" + itoa(i%120)
		lon := rng.Float64()*360 - 180
		lat := math.Asin(2*rng.Float64()-1) * 180 / math.Pi
		upsert(key, lon, lat)
	}
	// Delete about 70% to trigger repeated rebuilds.
	for i := 0; i < 90; i++ {
		del("k" + itoa(i))
	}

	pts := make([]geoRefPoint, 0, len(live))
	for _, p := range live {
		pts = append(pts, p)
	}
	for _, qc := range [][3]float64{{180, 0, 25}, {-180, 0, 25}, {0, 90, 40}, {0, -90, 5}} {
		want := refTopK(append([]geoRefPoint(nil), pts...), qc[0], qc[1], int(qc[2]), earthRadiusMeters)
		hits, err := runGeoQuery(t, db, "geo", qc[0], qc[1], int(qc[2]), GeoMeters)
		if err != nil {
			t.Fatal(err)
		}
		assertGeoHits(t, hits, want, earthRadiusMeters)
	}

	// Replacing a key with a new location updates the index.
	upsert("k100", 179.999, 0)
	hits, _ := runGeoQuery(t, db, "geo", 180, 0, 1, GeoMeters)
	if len(hits) != 1 || hits[0].key != "k100" {
		t.Fatalf("updated item should be nearest, got %v", hits)
	}
}

// TestNearbyGeoPruning verifies that queries visit substantially fewer
// nodes/leaves than a full scan, which is the requirement that pruning use
// a working lower bound rather than sorting a full enumeration.
func TestNearbyGeoPruning(t *testing.T) {
	const N = 4000
	db, _ := Open(":memory:")
	defer db.Close()
	db.CreateGeoIndex("geo", "*", IndexRect)
	rng := rand.New(rand.NewSource(99))
	db.Update(func(tx *Tx) error {
		for i := 0; i < N; i++ {
			lon := rng.Float64()*360 - 180
			lat := math.Asin(2*rng.Float64()-1) * 180 / math.Pi
			tx.Set("p"+itoa(i), Point(lon, lat), nil)
		}
		return nil
	})

	var idx *index
	db.View(func(tx *Tx) error {
		idx = tx.db.idxs["geo"]
		return nil
	})

	for _, qc := range [][3]float64{
		{180, 0, 10},
		{0, 90, 10},
		{12.5, -33.8, 20},
	} {
		_, err := runGeoQuery(t, db, "geo", qc[0], qc[1], int(qc[2]), GeoMeters)
		if err != nil {
			t.Fatal(err)
		}
		if idx.geoLeaves >= N {
			t.Fatalf("query (%.1f,%.1f) evaluated %d/%d leaves: no pruning",
				qc[0], qc[1], idx.geoLeaves, N)
		}
		if idx.geoBoxes >= N {
			t.Fatalf("query (%.1f,%.1f) expanded %d/%d boxes: no pruning",
				qc[0], qc[1], idx.geoBoxes, N)
		}
		// For a small k on a uniform sphere, a valid bound should prune the
		// large majority of the tree.
		if float64(idx.geoLeaves) > N*0.25 {
			t.Fatalf("query (%.1f,%.1f) evaluated %d/%d leaves: bound too weak",
				qc[0], qc[1], idx.geoLeaves, N)
		}
	}

	// After heavy deletes, pruning must still work on the rebuilt tree.
	db.Update(func(tx *Tx) error {
		for i := 0; i < N; i++ {
			if i%2 == 0 {
				tx.Delete("p" + itoa(i))
			}
		}
		return nil
	})
	_, _ = runGeoQuery(t, db, "geo", 180, 0, 5, GeoMeters)
	if idx.geoLeaves >= N/2 {
		t.Fatalf("post-delete pruning evaluated %d leaves", idx.geoLeaves)
	}
}

// TestGeoLowerBoundValid exhaustively checks that every subtree box's
// reported lower bound is no larger than the exact distance of every point
// it contains, for queries on the seam and at the poles. This is the
// mathematical safety net behind pruning.
func TestGeoLowerBoundValid(t *testing.T) {
	const N = 500
	tr := &geoTree{}
	rng := rand.New(rand.NewSource(123))
	var pts []geoPoint
	for i := 0; i < N; i++ {
		lon := rng.Float64()*360 - 180
		lat := math.Asin(2*rng.Float64()-1) * 180 / math.Pi
		dbi := &dbItem{key: "p" + itoa(i), val: Point(lon, lat)}
		pts = append(pts, geoPoint{lon: lon, lat: lat, item: dbi})
		tr.insert(dbi)
	}

	queries := [][2]float64{
		{180, 0}, {-180, 0}, {179.9, 1}, {-179.9, -1},
		{0, 90}, {0, -90}, {45, 89.999}, {-45, -89.999},
		{12.5, -33.8}, {0, 0},
	}
	for _, qc := range queries {
		q := &geoQuery{
			tree:   tr,
			lon:    qc[0],
			lat:    qc[1],
			radius: 1,
		}
		q.q[0] = geoUnitVector(qc[0], qc[1])
		q.q[1] = geoUnitVector(qc[0]+360, qc[1])
		q.q[2] = geoUnitVector(qc[0]-360, qc[1])
		var walk func(node *geoNode)
		walk = func(node *geoNode) {
			if node == nil || node.count == 0 {
				return
			}
			lb := q.boxLowerBound(node)
			if lb < 0 || lb > 4+1e-9 {
				t.Fatalf("lower bound out of range: %f", lb)
			}
			// Every live point in this subtree must be at least as far as
			// the bound.
			var check func(n *geoNode)
			check = func(n *geoNode) {
				if n == nil {
					return
				}
				if n.alive {
					exact := geoPointChordSquared(q, n.pt.lon, n.pt.lat)
					if lb > exact+1e-9 {
						t.Fatalf("invalid lower bound for query (%.3f,%.3f): "+
							"bound %.9f > exact %.9f at point (%.3f,%.3f)",
							q.lon, q.lat, lb, exact, n.pt.lon, n.pt.lat)
					}
				}
				check(n.left)
				check(n.right)
			}
			check(node)
			walk(node.left)
			walk(node.right)
		}
		walk(tr.root)
	}
}

// TestSpatialRectQueriesStillWork ensures the pre-existing arbitrary
// dimension rectangle queries are unchanged on standard spatial indexes.
func TestSpatialRectQueriesStillWork(t *testing.T) {
	db, _ := Open(":memory:")
	defer db.Close()
	db.CreateSpatialIndex("rects", "*", IndexRect)
	db.Update(func(tx *Tx) error {
		tx.Set("r1", "[0 0],[10 10]", nil)
		tx.Set("r2", "[20 20],[30 30]", nil)
		tx.Set("p1", Point(5, 5), nil)
		return nil
	})

	var got []string
	err := db.View(func(tx *Tx) error {
		return tx.Intersects("rects", "[4 4],[6 6]", func(key, value string) bool {
			got = append(got, key)
			return true
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "p1" || got[1] != "r1" {
		t.Fatalf("rectangle intersection mismatch: %v", got)
	}

	// Planar Nearby ordering is still box-distance based.
	var order []string
	db.View(func(tx *Tx) error {
		return tx.Nearby("rects", Point(0, 0), func(key, value string, dist float64) bool {
			order = append(order, key)
			return true
		})
	})
	if len(order) != 3 {
		t.Fatalf("planar Nearby returned %d items", len(order))
	}
	// p1 (point 5,5) and r1 (box [0 0],[10 10]) both have box distance 0
	// from the origin; r2 at (20,20) is last.
	if order[2] != "r2" {
		t.Fatalf("planar Nearby mismatch: %v", order)
	}
}

// BenchmarkNearbyGeo measures a top-10 spherical nearest-neighbor query on
// a uniform global point set.
func BenchmarkNearbyGeo_100k_K10(b *testing.B) {
	const N = 100000
	db, _ := Open(":memory:")
	defer db.Close()
	if err := db.CreateGeoIndex("geo", "*", IndexRect); err != nil {
		b.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	if err := db.Update(func(tx *Tx) error {
		for i := 0; i < N; i++ {
			lon := rng.Float64()*360 - 180
			lat := math.Asin(2*rng.Float64()-1) * 180 / math.Pi
			if _, _, err := tx.Set("p"+itoa(i), Point(lon, lat), nil); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		var seen int
		err := db.View(func(tx *Tx) error {
			return tx.NearbyGeo("geo", 139.69, 35.68, 10, GeoMeters,
				func(key, value string, dist float64) bool {
					seen++
					return true
				})
		})
		if err != nil {
			b.Fatal(err)
		}
		if seen != 10 {
			b.Fatalf("got %d results", seen)
		}
	}
}

// TestGeoIndexLifecycle covers the DB-level wrappers, ReplaceGeoIndex, a
// nil rect function (defaulting to GeoIndexRect), and transaction rollback
// rebuilding a dropped/created geo index.
func TestGeoIndexLifecycle(t *testing.T) {
	db, _ := Open(":memory:")
	defer db.Close()

	// nil rect defaults to GeoIndexRect.
	if err := db.CreateGeoIndex("geo", "*", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateGeoIndex("geo", "*", nil); err != ErrIndexExists {
		t.Fatalf("want ErrIndexExists, got %v", err)
	}

	if err := db.Update(func(tx *Tx) error {
		_, _, err := tx.Set("a", Point(1, 1), nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Rollback a DropIndex: the index is rebuilt with the item.
	if err := db.Update(func(tx *Tx) error {
		if err := tx.DropIndex("geo"); err != nil {
			return err
		}
		if _, ok := tx.db.idxs["geo"]; ok {
			t.Fatal("index should be dropped inside tx")
		}
		return errors.New("abort")
	}); err == nil || err.Error() != "abort" {
		t.Fatalf("want abort error, got %v", err)
	}
	hits, err := runGeoQuery(t, db, "geo", 0, 0, 10, GeoMeters)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].key != "a" {
		t.Fatalf("rolled-back drop should restore index, got %v", hits)
	}

	// Rollback a Set: the new point must not become visible.
	if err := db.Update(func(tx *Tx) error {
		_, _, _ = tx.Set("b", Point(2, 2), nil)
		return errors.New("abort")
	}); err == nil || err.Error() != "abort" {
		t.Fatalf("want abort error, got %v", err)
	}
	hits, _ = runGeoQuery(t, db, "geo", 0, 0, 10, GeoMeters)
	if len(hits) != 1 || hits[0].key != "a" {
		t.Fatalf("rolled-back set should not appear, got %v", hits)
	}

	// ReplaceGeoIndex rebuilds with a different pattern.
	if err := db.Update(func(tx *Tx) error {
		_, _, _ = tx.Set("keep:1", Point(3, 3), nil)
		_, _, _ = tx.Set("drop:1", Point(4, 4), nil)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceGeoIndex("geo", "keep:*", nil); err != nil {
		t.Fatal(err)
	}
	hits, _ = runGeoQuery(t, db, "geo", 0, 0, 10, GeoMeters)
	if len(hits) != 1 || hits[0].key != "keep:1" {
		t.Fatalf("replaced index should only match pattern, got %v", hits)
	}
}
