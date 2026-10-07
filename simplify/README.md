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

Road simplification often has to preserve intersections and toll/billing
points even when they are almost collinear with the surrounding road. The
`LineStringWithAnchors` / `RingWithAnchors` methods take a set of protected
vertex indices that are guaranteed to survive:

```go
original := orb.LineString{}
anchors := []int{3, 7} // intersections / billing points, kept by index

// reduced holds the simplified points and indexMap gives, for every reduced
// point, its index in the original linestring.
reduced, indexMap := simplify.DouglasPeucker(threshold).LineStringWithAnchors(original.Clone(), anchors)
```

-   Anchors are matched by **index**, not coordinate, so duplicate
    coordinates that represent different business points are never
    deduplicated into one.
-   The points between two consecutive anchors are still simplified using
    the configured threshold.
-   For closed rings the repeated closing point is kept and anchors that
    straddle the closure are honoured by simplifying the segments on either
    side of the seam independently.
-   If an anchor index is out of range the input is returned unchanged with a
    `nil` map; the input is not modified.
-   `DouglasPeuckerWithAnchors(threshold, ls, anchors)` is provided as a
    package-level convenience function.

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
