package talonflame

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/engelsjk/polygol"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/planar"
)

var testPark = orb.MultiPolygon{{{{9.97, 53.55}, {9.99, 53.55}, {9.99, 53.57}, {9.97, 53.57}, {9.97, 53.55}}}}

func TestMapCutsPreserveOSMPartition(t *testing.T) {
	park := orb.MultiPolygon{
		{{{9.97, 53.55}, {9.99, 53.55}, {9.99, 53.57}, {9.98, 53.57}, {9.98, 53.565}, {9.97, 53.565}, {9.97, 53.55}},
			{{9.972, 53.552}, {9.975, 53.552}, {9.975, 53.554}, {9.972, 53.554}, {9.972, 53.552}}},
		{{{9.97, 53.568}, {9.974, 53.568}, {9.974, 53.57}, {9.97, 53.57}, {9.97, 53.568}}},
	}
	populations := []mapPopulation{
		{pair: Pair{16, 0}, points: []orb.Point{{9.974, 53.56}, {9.972, 53.569}}},
		{pair: Pair{276, 0}, points: []orb.Point{{9.986, 53.556}, {9.986, 53.56}}},
		{pair: Pair{769, 0}, points: []orb.Point{{9.986, 53.567}}},
	}
	regions, err := splitMapRegions(park, populations)
	if err != nil {
		t.Fatal(err)
	}
	if len(regions) != 3 {
		t.Fatal("missing populations", len(regions))
	}
	var parts []polygol.Geom
	for _, p := range regions {
		parts = append(parts, clipGeometry(p))
		if planar.MultiPolygonContains(p, orb.Point{9.973, 53.553}) {
			t.Fatal("filled OSM hole")
		}
		anchor := mapAnchor(p, orb.Point{0, 0})
		if !planar.MultiPolygonContains(p, anchor) {
			t.Fatal("marker outside region")
		}
	}
	combined, err := polygol.Union(parts[0], parts[1:]...)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := polygol.Difference(clipGeometry(park), combined)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(planar.Area(orbGeometry(missing))) > 1e-12 {
		t.Fatal("park has gaps")
	}
	extra, err := polygol.Difference(combined, clipGeometry(park))
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(planar.Area(orbGeometry(extra))) > 1e-12 {
		t.Fatal("cut extended beyond OSM boundary")
	}
	for i := range parts {
		for j := i + 1; j < len(parts); j++ {
			overlap, err := polygol.Intersection(parts[i], parts[j])
			if err != nil {
				t.Fatal(err)
			}
			if math.Abs(planar.Area(orbGeometry(overlap))) > 1e-12 {
				t.Fatal("overlapping interiors")
			}
		}
	}
}

func TestMapSinglePopulationKeepsOriginalRings(t *testing.T) {
	regions, err := splitMapRegions(testPark, []mapPopulation{{pair: Pair{16, 0}}})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(regions[Pair{16, 0}])
	b, _ := json.Marshal(testPark)
	if string(a) != string(b) {
		t.Fatal("single population changed OSM outline")
	}
}

func TestMapTwoSpeciesHaveOneStraightCut(t *testing.T) {
	regions, err := splitMapRegions(testPark, []mapPopulation{
		{pair: Pair{16, 0}, points: []orb.Point{{9.974, 53.555}, {9.974, 53.565}}},
		{pair: Pair{769, 0}, points: []orb.Point{{9.986, 53.555}, {9.986, 53.565}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var cutPoints []orb.Point
	for _, p := range regions[Pair{16, 0}] {
		for _, ring := range p {
			for _, v := range ring {
				if v[0] > 9.97 && v[0] < 9.99 {
					cutPoints = append(cutPoints, v)
				}
			}
		}
	}
	if len(cutPoints) < 2 {
		t.Fatal("missing straight cut")
	}
	for _, v := range cutPoints {
		if math.Abs(v[0]-9.98) > 1e-9 {
			t.Fatal("cut is not straight", v)
		}
	}
}

func fixtureMapSource(id int64, pair Pair) *mapSource {
	s := &mapSource{id: id, name: "Park", region: testPark, center: orb.Point{9.98, 53.56}, count: 10}
	s.parts = []mapPart{{owner: id, aliases: []int64{id}, pair: pair, region: s.region, count: s.count, source: s}}
	return s
}

func TestMapSnapshotsReuseUnchangedEntriesAndTrackAllChanges(t *testing.T) {
	a, b := fixtureMapSource(1, Pair{16, 0}), fixtureMapSource(2, Pair{276, 0})
	sources := map[int64]*mapSource{1: a, 2: b}
	first, err := buildMapSnapshot(sources, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildMapSnapshot(sources, first)
	if err != nil {
		t.Fatal(err)
	}
	if second.revision != first.revision || second.entries["1"] != first.entries["1"] {
		t.Fatal("unchanged geometry was rebuilt")
	}
	changed := fixtureMapSource(1, Pair{769, 0})
	changed.name = "Renamed park"
	third, err := buildMapSnapshot(map[int64]*mapSource{1: changed, 2: b}, second)
	if err != nil {
		t.Fatal(err)
	}
	if third.revision == second.revision || third.entries["2"] != second.entries["2"] {
		t.Fatal("change invalidation or reuse failed")
	}
	if first.entries["1"].Pokemon != 16 || first.entries["1"].Name != "Park" {
		t.Fatal("mutated published snapshot")
	}
	withdrawn, err := buildMapSnapshot(map[int64]*mapSource{2: b}, third)
	if err != nil {
		t.Fatal(err)
	}
	if withdrawn.entries["1"] != nil || withdrawn.revision == third.revision {
		t.Fatal("withdrawal not represented")
	}
	geometryChanged := fixtureMapSource(1, Pair{16, 0})
	geometryChanged.region = orb.MultiPolygon{{{{9.97, 53.55}, {9.98, 53.55}, {9.98, 53.57}, {9.97, 53.57}, {9.97, 53.55}}}}
	geometryChanged.parts[0].region = geometryChanged.region
	fourth, err := buildMapSnapshot(map[int64]*mapSource{1: geometryChanged, 2: b}, first)
	if err != nil {
		t.Fatal(err)
	}
	if fourth.revision == first.revision {
		t.Fatal("geometry change omitted from revision")
	}
}

func TestMapAPIIsBoundedConditionalAndSnapshotBased(t *testing.T) {
	source := fixtureMapSource(1, Pair{16, 0})
	source.parts[0].aliases = append(source.parts[0].aliases, -77)
	snapshot, err := buildMapSnapshot(map[int64]*mapSource{1: source, 2: fixtureMapSource(2, Pair{276, 0})}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := newMapStore(nil, nil)
	m.current.Store(snapshot)
	m.lastSuccess.Store(time.Now().Unix())
	request := func(path, etag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("If-None-Match", etag)
		w := httptest.NewRecorder()
		m.serveHTTP(w, r)
		return w
	}
	url := "/api/map/nests?bbox=9.97,53.55,9.99,53.57"
	w := request(url, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var response mapResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Snapshot || len(response.Data) != 2 || response.Revision == "" {
		t.Fatal("not a current snapshot")
	}
	if next := request(url, w.Header().Get("ETag")); next.Code != 304 || next.Body.Len() != 0 {
		t.Fatal("conditional request sent geometry")
	}
	limited := request(url+"&limit=1", "")
	json.Unmarshal(limited.Body.Bytes(), &response)
	if !response.LimitReached || len(response.Data) != 0 {
		t.Fatal("unbounded result")
	}
	filtered := request(url+"&pokemon=16", "")
	json.Unmarshal(filtered.Body.Bytes(), &response)
	if len(response.Data) != 1 {
		t.Fatal("species filter ignored")
	}
	alias := request("/api/map/nests?id=-77", "")
	if alias.Code != 200 || !strings.Contains(alias.Body.String(), `"id":"-77"`) {
		t.Fatal("negative alias broken", alias.Body.String())
	}
	for _, path := range []string{"/api/map/nests", "/api/map/nests?bbox=NaN,0,1,1", "/api/map/nests?bbox=0,0,1,1&limit=10001", "/api/map/nests?id=bad"} {
		if request(path, "").Code != 400 {
			t.Fatal("invalid query accepted", path)
		}
	}
	empty, err := buildMapSnapshot(map[int64]*mapSource{}, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	m.current.Store(empty)
	removed := request(url, w.Header().Get("ETag"))
	if removed.Code != 200 {
		t.Fatal("withdrawal incorrectly returned 304")
	}
	json.Unmarshal(removed.Body.Bytes(), &response)
	if len(response.Data) != 0 || !response.Snapshot {
		t.Fatal("withdrawn nests retained")
	}
	if m.builds.Load() != 0 {
		t.Fatal("HTTP request performed geometry build")
	}
}

func TestMapAPIConcurrentSnapshotPublication(t *testing.T) {
	a, err := buildMapSnapshot(map[int64]*mapSource{1: fixtureMapSource(1, Pair{16, 0})}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildMapSnapshot(map[int64]*mapSource{}, a)
	if err != nil {
		t.Fatal(err)
	}
	m := newMapStore(nil, nil)
	m.current.Store(a)
	m.lastSuccess.Store(time.Now().Unix())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				w := httptest.NewRecorder()
				m.serveHTTP(w, httptest.NewRequest("GET", "/api/map/nests?bbox=9,53,10,54", nil))
				if w.Code != 200 {
					t.Error(w.Code)
				}
			}
		}()
	}
	for i := 0; i < 30; i++ {
		m.current.Store(b)
		m.current.Store(a)
	}
	wg.Wait()
	m.invalidate(1, 2)
	m.invalidate(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.dirty) != 2 {
		t.Fatal("invalidations not coalesced")
	}
}

func TestMapOnlyListenerDoesNotExposeWebhooks(t *testing.T) {
	handler := NewServer(&Analyzer{}).MapHandler()
	for _, path := range []string{"/webhook", "/api/nests", "/metrics"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader("[]")))
		if w.Code != 404 {
			t.Fatal("map-only listener exposed", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/map/nests?id=1", nil))
	if w.Code != 503 {
		t.Fatal("map route missing while snapshot is initializing", w.Code)
	}
}

func TestMapAliasesIncludeAllLinkedParks(t *testing.T) {
	a, b := fixtureMapSource(1, Pair{276, 0}), fixtureMapSource(2, Pair{276, 0})
	a.parts[0].aliases = []int64{1, -77}
	b.parts[0].aliases = []int64{2, -77}
	b.region = orb.MultiPolygon{{{{10, 54}, {10.01, 54}, {10.01, 54.01}, {10, 54.01}, {10, 54}}}}
	b.parts[0].region = b.region
	s, err := buildMapSnapshot(map[int64]*mapSource{1: a, 2: b}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.entries) != 1 || s.aliases["-77"] != "1" || s.aliases["2"] != "1" {
		t.Fatal("same-species alias component fragmented")
	}
	if len(s.entries["1"].region) != 2 {
		t.Fatal("direct ID lost one linked park")
	}
}

func BenchmarkMapAPIUnchangedViewport(b *testing.B) {
	sources := map[int64]*mapSource{}
	for i := int64(1); i <= 10000; i++ {
		sources[i] = fixtureMapSource(i, Pair{16, 0})
	}
	snapshot, err := buildMapSnapshot(sources, nil)
	if err != nil {
		b.Fatal(err)
	}
	m := newMapStore(nil, nil)
	m.current.Store(snapshot)
	m.lastSuccess.Store(time.Now().Unix())
	r := httptest.NewRequest("GET", "/api/map/nests?bbox=9.97,53.55,9.99,53.57&limit=100", nil)
	w := httptest.NewRecorder()
	m.serveHTTP(w, r)
	r.Header.Set("If-None-Match", w.Header().Get("ETag"))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		m.serveHTTP(w, r)
		if w.Code != 304 {
			b.Fatal(fmt.Sprint(w.Code))
		}
	}
}
