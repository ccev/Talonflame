package talonflame

import (
	"fmt"
	"math"
	"sort"

	"github.com/engelsjk/polygol"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/planar"
)

type mapPopulation struct {
	pair   Pair
	groups []int64
	points []orb.Point
}

func polygonGeometry(g orb.Geometry) (orb.MultiPolygon, error) {
	switch p := g.(type) {
	case orb.Polygon:
		return orb.MultiPolygon{p}, nil
	case orb.MultiPolygon:
		return p, nil
	default:
		return nil, fmt.Errorf("expected polygon, got %T", g)
	}
}

func clipGeometry(p orb.MultiPolygon) polygol.Geom {
	g := make(polygol.Geom, len(p))
	for i, polygon := range p {
		g[i] = make([][][]float64, len(polygon))
		for j, ring := range polygon {
			g[i][j] = make([][]float64, len(ring))
			for k, point := range ring {
				g[i][j][k] = []float64{point[0], point[1]}
			}
		}
	}
	return g
}

func orbGeometry(g polygol.Geom) orb.MultiPolygon {
	p := make(orb.MultiPolygon, len(g))
	for i, polygon := range g {
		p[i] = make(orb.Polygon, len(polygon))
		for j, ring := range polygon {
			p[i][j] = make(orb.Ring, len(ring))
			for k, point := range ring {
				p[i][j][k] = orb.Point{point[0], point[1]}
			}
		}
	}
	return p
}

// Split an OSM geofence with a single half-plane. The other half is its exact
// complement, preserving concavities, holes and disconnected park sections.
func cutMapRegion(region orb.MultiPolygon, origin, normal orb.Point, cut float64) (orb.MultiPolygon, orb.MultiPolygon, error) {
	b := region.Bound()
	margin := math.Max(math.Max(b.Max[0]-b.Min[0], b.Max[1]-b.Min[1]), .000001)
	corners := []orb.Point{{b.Min[0] - margin, b.Min[1] - margin}, {b.Max[0] + margin, b.Min[1] - margin}, {b.Max[0] + margin, b.Max[1] + margin}, {b.Min[0] - margin, b.Max[1] + margin}}
	distance := func(p orb.Point) float64 { return (p[0]-origin[0])*normal[0] + (p[1]-origin[1])*normal[1] - cut }
	var ring orb.Ring
	for i, a := range corners {
		b := corners[(i+1)%len(corners)]
		da, db := distance(a), distance(b)
		if da <= 0 {
			ring = append(ring, a)
		}
		if (da < 0 && db > 0) || (da > 0 && db < 0) {
			t := da / (da - db)
			ring = append(ring, orb.Point{a[0] + t*(b[0]-a[0]), a[1] + t*(b[1]-a[1])})
		}
	}
	if len(ring) < 3 {
		return nil, region, nil
	}
	ring = append(ring, ring[0])
	g := clipGeometry(region)
	left, err := polygol.Intersection(g, clipGeometry(orb.MultiPolygon{{ring}}))
	if err != nil {
		return nil, nil, err
	}
	if len(left) == 0 {
		return nil, region, nil
	}
	right, err := polygol.Difference(g, left)
	return orbGeometry(left), orbGeometry(right), err
}

func splitMapRegions(region orb.MultiPolygon, populations []mapPopulation) (map[Pair]orb.MultiPolygon, error) {
	if len(populations) == 1 {
		return map[Pair]orb.MultiPolygon{populations[0].pair: region}, nil
	}
	centers := make([]orb.Point, len(populations))
	for i, p := range populations {
		for _, v := range p.points {
			centers[i][0] += v[0] / float64(len(p.points))
			centers[i][1] += v[1] / float64(len(p.points))
		}
	}
	scale := math.Max(.000001, math.Cos(centers[0][1]*math.Pi/180))
	normal := orb.Point{1, 0}
	furthest := 0.0
	for i := range centers {
		for j := i + 1; j < len(centers); j++ {
			dx, dy := (centers[j][0]-centers[i][0])*scale, centers[j][1]-centers[i][1]
			if d := dx*dx + dy*dy; d > furthest {
				furthest = d
				normal = orb.Point{dx * scale, dy}
			}
		}
	}
	origin := centers[0]
	project := func(p orb.Point) float64 { return (p[0]-origin[0])*normal[0] + (p[1]-origin[1])*normal[1] }
	type projected struct {
		population mapPopulation
		value      float64
	}
	ordered := make([]projected, len(populations))
	for i, p := range populations {
		ordered[i] = projected{p, project(centers[i])}
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].value != ordered[j].value {
			return ordered[i].value < ordered[j].value
		}
		return ordered[i].population.pair.String() < ordered[j].population.pair.String()
	})
	gap := 0
	for i := 1; i < len(ordered)-1; i++ {
		if ordered[i+1].value-ordered[i].value > ordered[gap+1].value-ordered[gap].value {
			gap = i
		}
	}
	midpoint := (ordered[gap].value + ordered[gap+1].value) / 2
	leftPairs := map[Pair]bool{}
	for _, p := range ordered[:gap+1] {
		leftPairs[p.population.pair] = true
	}
	type vote struct {
		value, weight float64
		left          bool
	}
	var votes []vote
	for _, p := range populations {
		for _, v := range p.points {
			votes = append(votes, vote{project(v), 1 / float64(len(p.points)), leftPairs[p.pair]})
		}
	}
	sort.Slice(votes, func(i, j int) bool { return votes[i].value < votes[j].value })
	cut, best := midpoint, 0.0
	for _, v := range votes {
		if (v.value <= cut) != v.left {
			best += v.weight
		}
	}
	errorWeight := float64(len(leftPairs))
	for i := 0; i < len(votes)-1; i++ {
		if votes[i].left {
			errorWeight -= votes[i].weight
		} else {
			errorWeight += votes[i].weight
		}
		candidate := (votes[i].value + votes[i+1].value) / 2
		if candidate <= ordered[gap].value || candidate >= ordered[gap+1].value {
			continue
		}
		if errorWeight < best-1e-10 || (math.Abs(errorWeight-best) <= 1e-10 && math.Abs(candidate-midpoint) < math.Abs(cut-midpoint)) {
			cut, best = candidate, errorWeight
		}
	}
	left, right, err := cutMapRegion(region, origin, normal, cut)
	if err != nil {
		return nil, err
	}
	if len(left) == 0 || len(right) == 0 {
		owner, maxCount := populations[0].pair, -1
		for _, p := range populations {
			count := 0
			for _, v := range p.points {
				if planar.MultiPolygonContains(region, v) {
					count++
				}
			}
			if count > maxCount {
				owner, maxCount = p.pair, count
			}
		}
		return map[Pair]orb.MultiPolygon{owner: region}, nil
	}
	var lp, rp []mapPopulation
	for i, p := range ordered {
		if i <= gap {
			lp = append(lp, p.population)
		} else {
			rp = append(rp, p.population)
		}
	}
	result, err := splitMapRegions(left, lp)
	if err != nil {
		return nil, err
	}
	rest, err := splitMapRegions(right, rp)
	if err != nil {
		return nil, err
	}
	for pair, p := range rest {
		result[pair] = p
	}
	return result, nil
}

func mapAnchor(region orb.MultiPolygon, preferred orb.Point) orb.Point {
	if planar.MultiPolygonContains(region, preferred) {
		return preferred
	}
	center, _ := planar.CentroidArea(region)
	if planar.MultiPolygonContains(region, center) {
		return center
	}
	// Find an interior point on a horizontal scanline, including around holes.
	for _, p := range region {
		if len(p) == 0 {
			continue
		}
		b := p.Bound()
		y := (b.Min[1] + b.Max[1]) / 2
		var xs []float64
		for _, ring := range p {
			for i := 1; i < len(ring); i++ {
				a, b := ring[i-1], ring[i]
				if (a[1] > y) != (b[1] > y) {
					xs = append(xs, a[0]+(y-a[1])*(b[0]-a[0])/(b[1]-a[1]))
				}
			}
		}
		sort.Float64s(xs)
		for i := 1; i < len(xs); i++ {
			v := orb.Point{(xs[i-1] + xs[i]) / 2, y}
			if planar.PolygonContains(p, v) {
				return v
			}
		}
	}
	return region[0][0][0]
}
