# orb/simplify [![Godoc Reference](https://pkg.go.dev/badge/github.com/paulmach/orb)](https://pkg.go.dev/github.com/paulmach/orb/simplify)

This package implements several reducing/simplifying function for `orb.Geometry` types.

Currently implemented:

-   [Douglas-Peucker](#dp)
-   [Visvalingam](#vis)
-   [Radial](#radial)

**Note:** The geometry object CAN be modified, use `Clone()` if a copy is required.

## <a name="dp"></a>Douglas-Peucker

Probably the most popular simplification algorithm. For algorithm details, see
[wikipedia](http://en.wikipedia.org/wiki/Ramer%E2%80%93Douglas%E2%80%93Peucker_algorithm).

The algorithm is a pass through for 1d geometry, e.g. Point and MultiPoint.
The algorithms can modify the original geometry, use `Clone()` if a copy is required.

Usage:

    original := orb.LineString{}
    reduced := simplify.DouglasPeucker(threshold).Simplify(original.Clone())

### Protected vertices (anchors)

When simplifying roads some vertices, such as intersections or toll points,
must be preserved even when they are nearly collinear with their neighbours.
`LineStringWithAnchors` and `RingWithAnchors` accept the indices of these
protected vertices. The anchors are mandatory break points, while the points
between consecutive anchors are still simplified subject to the threshold.
Both methods return the simplified geometry and an index map giving the
original index of every output point.

Anchors are positions in the input slice, not coordinates, so duplicate
coordinates carrying different business indices are handled correctly. If an
anchor index is out of range the input is returned unmodified. For closed
rings the shared start/end point is always retained and anchors may straddle
the closing edge.

```go
original := orb.LineString{}
anchors := []int{12, 37}

reduced, indexMap := simplify.DouglasPeucker(threshold).
    LineStringWithAnchors(original, anchors)

reducedRing, ringIndexMap := simplify.DouglasPeucker(threshold).
    RingWithAnchors(ring, anchors)
```

## <a name="vis"></a>Visvalingam

See Mike Bostock's explanation for
[algorithm details](http://bost.ocks.org/mike/simplify/).

The algorithm is a pass through for 1d geometry, e.g. Point and MultiPoint.
The algorithms can modify the original geometry, use `Clone()` if a copy is required.

Usage:

```go
original := orb.Ring{}

// will remove all whose triangle is smaller than `threshold`
reduced := simplify.VisvalingamThreshold(threshold).Simplify(original)

// will remove points until there are only `toKeep` points left.
reduced := simplify.VisvalingamKeep(toKeep).Simplify(original)

// One can also combine the parameters.
// This will continue to remove points until:
//  - there are no more below the threshold,
//  - or the new path is of length `toKeep`
reduced := simplify.Visvalingam(threshold, toKeep).Simplify(original)
```

## <a name="radial"></a>Radial

Radial reduces the path by removing points that are close together.
A full [algorithm description](http://psimpl.sourceforge.net/radial-distance.html).

The algorithm is a pass through for 1d geometry, like Point and MultiPoint.
The algorithms can modify the original geometry, use `Clone()` if a copy is required.

Usage:

```go
original := geo.Polygon{}

// this method uses a Euclidean distance measure.
reduced := simplify.Radial(planar.Distance, threshold).Simplify(path)

// if the points are in the lng/lat space Radial Geo will
// compute the geo distance between the coordinates.
reduced:= simplify.Radial(geo.Distance, meters).Simplify(path)
```
