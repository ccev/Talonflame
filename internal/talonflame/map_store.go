package talonflame

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/engelsjk/polygol"
	"github.com/golang/geo/s2"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/geo"
	"github.com/paulmach/orb/geojson"
	"github.com/paulmach/orb/planar"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/rtree"
)

// MapNest is an immutable display record. Geometry is serialized once at build
// time, never reconstructed by an HTTP request or by the webhook worker.
type MapNest struct {
	ID          string          `json:"id"`
	Pokemon     int             `json:"pokemon_id"`
	Form        int             `json:"form"`
	Name        string          `json:"name"`
	AreaName    string          `json:"area_name"`
	Lat         float64         `json:"lat"`
	Lon         float64         `json:"lon"`
	Spawnpoints int             `json:"spawnpoints"`
	Area        float64         `json:"m2"`
	LastChanged *int64          `json:"last_changed"`
	Geometry    json.RawMessage `json:"geometry"`
	region      orb.MultiPolygon
	encoded     json.RawMessage
	parts       []mapPart
	digest      [32]byte
}

type mapSnapshot struct {
	revision string
	entries  map[string]*MapNest
	aliases  map[string]string
	tree     rtree.RTreeG[*MapNest]
	stats    []mapStat
}
type mapStat struct {
	Pokemon int `json:"pokemon_id"`
	Form    int `json:"form"`
	Count   int `json:"count"`
}
type mapSource struct {
	id             int64
	name, areaName string
	center         orb.Point
	count          int
	region         orb.MultiPolygon
	groups         []Group
	parts          []mapPart
}
type mapPart struct {
	owner   int64
	aliases []int64
	pair    Pair
	region  orb.MultiPolygon
	count   int
	changed *int64
	source  *mapSource
}

type mapStore struct {
	db          *sql.DB
	log         *logrus.Logger
	current     atomic.Pointer[mapSnapshot]
	mu          sync.Mutex
	dirty       map[int64]bool
	sources     map[int64]*mapSource
	signatures  map[int64]string
	builds      atomic.Uint64
	lastSuccess atomic.Int64
}

func newMapStore(db *sql.DB, logger *logrus.Logger) *mapStore {
	return &mapStore{db: db, log: logger, dirty: map[int64]bool{}, sources: map[int64]*mapSource{}, signatures: map[int64]string{}}
}
func (m *mapStore) invalidate(ids ...int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		m.dirty[id] = true
	}
}

// A single background builder coalesces analyzer notifications. The minute
// sweep detects external source geometry/name/active changes, including deletes.
func (m *mapStore) run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	nextSweep := time.Time{}
	for {
		if m.db == nil {
			return
		}
		if err := m.refresh(ctx, !time.Now().Before(nextSweep)); err != nil {
			if ctx.Err() != nil {
				return
			}
			m.log.WithError(err).Error("nest map snapshot refresh failed")
		} else if !time.Now().Before(nextSweep) {
			nextSweep = time.Now().Add(time.Minute)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *mapStore) scanSources(ctx context.Context) error {
	rows, err := m.db.QueryContext(ctx, `SELECT n.nest_id,n.name,n.area_name,n.spawnpoints,n.lat,n.lon,
 SHA2(ST_AsWKB(n.polygon),256),g.nest_id,g.pokemon_id,g.pokemon_form,g.last_changed
 FROM nests n JOIN talonflame_nest_sources s ON s.source_nest_id=n.nest_id
 JOIN talonflame_nest_groups g ON g.nest_id=s.group_id
 WHERE n.active=1 AND g.status='confirmed' AND g.pokemon_id IS NOT NULL
 ORDER BY n.nest_id,g.nest_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	next := map[int64]string{}
	var prior int64
	h := sha256.New()
	for rows.Next() {
		var id, gid int64
		var name, area, geometry sql.NullString
		var count, pokemon, form, changed sql.NullInt64
		var lat, lon sql.NullFloat64
		if err := rows.Scan(&id, &name, &area, &count, &lat, &lon, &geometry, &gid, &pokemon, &form, &changed); err != nil {
			return err
		}
		if prior != 0 && prior != id {
			next[prior] = hex.EncodeToString(h.Sum(nil))
			h.Reset()
		}
		fmt.Fprintf(h, "%d:%q:%q:%v:%v:%v:%s:%d:%v:%v:%v;", id, name.String, area.String, count, lat, lon, geometry.String, gid, pokemon, form, changed)
		prior = id
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if prior != 0 {
		next[prior] = hex.EncodeToString(h.Sum(nil))
	}
	for id, digest := range next {
		if m.signatures[id] != digest {
			m.invalidate(id)
		}
	}
	for id := range m.signatures {
		if _, ok := next[id]; !ok {
			m.invalidate(id)
		}
	}
	m.signatures = next
	return nil
}

func (m *mapStore) refresh(ctx context.Context, sweep bool) error {
	if sweep {
		if err := m.scanSources(ctx); err != nil {
			return err
		}
	}
	m.mu.Lock()
	ids := make([]int64, 0, len(m.dirty))
	for id := range m.dirty {
		ids = append(ids, id)
	}
	m.dirty = map[int64]bool{}
	m.mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) == 0 && m.current.Load() != nil {
		m.lastSuccess.Store(time.Now().Unix())
		return nil
	}
	// Stage replacements. A failed query or split keeps the old complete
	// snapshot and requeues every affected source for the next attempt.
	replacements := map[int64]*mapSource{}
	for start := 0; start < len(ids); start += 500 {
		end := start + 500
		if end > len(ids) {
			end = len(ids)
		}
		batch, err := m.loadSources(ctx, ids[start:end])
		if err != nil {
			m.invalidate(ids...)
			return err
		}
		for id, source := range batch {
			replacements[id] = source
		}
	}
	previous := m.sources
	nextSources := make(map[int64]*mapSource, len(previous)+len(replacements))
	for id, source := range previous {
		nextSources[id] = source
	}
	for _, id := range ids {
		delete(nextSources, id)
	}
	for id, source := range replacements {
		nextSources[id] = source
	}
	snapshot, err := buildMapSnapshot(nextSources, m.current.Load())
	if err != nil {
		m.invalidate(ids...)
		return err
	}
	m.sources = nextSources
	old := m.current.Swap(snapshot)
	m.lastSuccess.Store(time.Now().Unix())
	m.builds.Add(1)
	if old == nil || old.revision != snapshot.revision {
		m.log.WithFields(logrus.Fields{"nests": len(snapshot.entries), "sources_rebuilt": len(ids), "revision": snapshot.revision}).Info("nest map snapshot published")
	}
	return nil
}

func (m *mapStore) loadSources(ctx context.Context, ids []int64) (map[int64]*mapSource, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	tx, err := m.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT n.nest_id,n.name,n.area_name,n.lat,n.lon,n.spawnpoints,ST_AsGeoJSON(n.polygon),
 g.nest_id,g.pokemon_id,g.pokemon_form,g.last_changed
 FROM nests n JOIN talonflame_nest_sources s ON s.source_nest_id=n.nest_id
 JOIN talonflame_nest_groups g ON g.nest_id=s.group_id
 WHERE n.active=1 AND g.status='confirmed' AND g.pokemon_id IS NOT NULL
 AND n.nest_id IN (`+placeholders+`) ORDER BY n.nest_id,g.nest_id`, args...)
	if err != nil {
		return nil, err
	}
	result := map[int64]*mapSource{}
	for rows.Next() {
		var id int64
		var name, area sql.NullString
		var lat, lon sql.NullFloat64
		var count, form, changed sql.NullInt64
		var raw []byte
		var group Group
		if err := rows.Scan(&id, &name, &area, &lat, &lon, &count, &raw, &group.ID, &group.Pair.Pokemon, &form, &changed); err != nil {
			rows.Close()
			return nil, err
		}
		source := result[id]
		if source == nil {
			g, err := geojson.UnmarshalGeometry(raw)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("source %d: %w", id, err)
			}
			region, err := polygonGeometry(g.Geometry())
			if err != nil {
				rows.Close()
				return nil, err
			}
			source = &mapSource{id: id, name: name.String, areaName: area.String, center: orb.Point{lon.Float64, lat.Float64}, count: int(count.Int64), region: region}
			result[id] = source
		}
		group.Pair.Form = int(form.Int64)
		group.Confirmed = true
		if changed.Valid {
			value := changed.Int64
			group.LastChanged = &value
		}
		source.groups = append(source.groups, group)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Single-population parks keep the OSM geometry without reading millions
	// of imported spawnpoints. Only mixed parks need membership for fitting cuts.
	mixed := map[int64]bool{}
	var mixedArgs []any
	for id, source := range result {
		pairs := map[Pair]bool{}
		for _, g := range source.groups {
			pairs[g.Pair] = true
		}
		if len(pairs) > 1 {
			mixed[id] = true
			mixedArgs = append(mixedArgs, id)
		}
	}
	points := map[int64]map[int64][]orb.Point{}
	if len(mixedArgs) > 0 {
		rows, err = tx.QueryContext(ctx, `SELECT s.source_nest_id,p.nest_id,p.spawnpoint_id FROM talonflame_nest_sources s
 JOIN talonflame_spawnpoint_state p ON p.nest_id=s.group_id
 WHERE s.source_nest_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(mixedArgs)), ",")+`) ORDER BY s.source_nest_id,p.nest_id,p.spawnpoint_id`, mixedArgs...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var source, group int64
			var token uint64
			if err := rows.Scan(&source, &group, &token); err != nil {
				rows.Close()
				return nil, err
			}
			cell := s2.CellID(token << 20)
			if token >= 1<<44 || !cell.IsValid() || cell.Level() != 20 {
				continue
			}
			ll := cell.LatLng()
			p := orb.Point{ll.Lng.Degrees(), ll.Lat.Degrees()}
			if !planar.MultiPolygonContains(result[source].region, p) {
				continue
			}
			if points[source] == nil {
				points[source] = map[int64][]orb.Point{}
			}
			points[source][group] = append(points[source][group], p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for id, source := range result {
		populations := map[Pair]*mapPopulation{}
		for _, g := range source.groups {
			p := populations[g.Pair]
			if p == nil {
				p = &mapPopulation{pair: g.Pair}
				populations[g.Pair] = p
			}
			p.groups = append(p.groups, g.ID)
			p.points = append(p.points, points[id][g.ID]...)
		}
		var active []mapPopulation
		for _, p := range populations {
			if len(populations) == 1 || len(p.points) > 0 {
				active = append(active, *p)
			}
		}
		sort.Slice(active, func(i, j int) bool { return active[i].pair.String() < active[j].pair.String() })
		if len(active) == 0 {
			continue
		}
		regions, err := splitMapRegions(source.region, active)
		if err != nil {
			return nil, fmt.Errorf("source %d split: %w", id, err)
		}
		for _, p := range active {
			region := regions[p.pair]
			if len(region) == 0 {
				continue
			}
			sort.Slice(p.groups, func(i, j int) bool {
				if p.groups[i] == id {
					return true
				}
				if p.groups[j] == id {
					return false
				}
				return p.groups[i] < p.groups[j]
			})
			count := source.count
			if mixed[id] {
				count = 0
				for _, v := range p.points {
					if planar.MultiPolygonContains(region, v) {
						count++
					}
				}
			}
			var changed *int64
			for _, g := range source.groups {
				if g.ID == p.groups[0] {
					changed = g.LastChanged
				}
			}
			source.parts = append(source.parts, mapPart{p.groups[0], p.groups, p.pair, region, count, changed, source})
		}
	}
	return result, nil
}

func buildMapSnapshot(sources map[int64]*mapSource, old *mapSnapshot) (*mapSnapshot, error) {
	byGroup := map[int64][]mapPart{}
	// Same-species groups may share several overlapping sources. Resolve their
	// aliases globally so every direct ID gets the complete precomputed region,
	// rather than whichever park happened to overwrite its alias last.
	parents := map[int64]int64{}
	var root func(int64) int64
	root = func(id int64) int64 {
		parent, ok := parents[id]
		if !ok {
			parents[id] = id
			return id
		}
		if parent != id {
			parents[id] = root(parent)
		}
		return parents[id]
	}
	var sourceIDs []int64
	for id := range sources {
		sourceIDs = append(sourceIDs, id)
	}
	sort.Slice(sourceIDs, func(i, j int) bool { return sourceIDs[i] < sourceIDs[j] })
	for _, id := range sourceIDs {
		for _, part := range sources[id].parts {
			for _, alias := range part.aliases {
				a, b := root(part.owner), root(alias)
				if a == b {
					continue
				}
				if (a < 0 && b > 0) || ((a > 0) == (b > 0) && b < a) {
					a, b = b, a
				}
				parents[b] = a
			}
		}
	}
	for _, id := range sourceIDs {
		for _, p := range sources[id].parts {
			owner := root(p.owner)
			byGroup[owner] = append(byGroup[owner], p)
		}
	}
	s := &mapSnapshot{entries: map[string]*MapNest{}, aliases: map[string]string{}, stats: []mapStat{}}
	var ids []int64
	for id := range byGroup {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	h := sha256.New()
	counts := map[Pair]int{}
	for _, id := range ids {
		parts := byGroup[id]
		key := strconv.FormatInt(id, 10)
		// Reuse unchanged entries and unions by comparing immutable source parts.
		var entry *MapNest
		if old != nil {
			candidate := old.entries[key]
			if candidate != nil && len(candidate.parts) == len(parts) {
				unchanged := true
				for i, p := range parts {
					if p.source != candidate.parts[i].source {
						unchanged = false
						break
					}
				}
				if unchanged {
					entry = candidate
				}
			}
		}
		if entry == nil {
			region := parts[0].region
			if len(parts) > 1 {
				geoms := make([]polygol.Geom, len(parts)-1)
				for i, p := range parts[1:] {
					geoms[i] = clipGeometry(p.region)
				}
				g, err := polygol.Union(clipGeometry(region), geoms...)
				if err != nil {
					return nil, err
				}
				region = orbGeometry(g)
			}
			if len(region) == 0 {
				continue
			}
			anchor := mapAnchor(region, parts[0].source.center)
			raw, err := json.Marshal(geojson.NewGeometry(region))
			if err != nil {
				return nil, err
			}
			candidate := &MapNest{ID: key, Pokemon: parts[0].pair.Pokemon, Form: parts[0].pair.Form, Name: parts[0].source.name, AreaName: parts[0].source.areaName, Lat: anchor[1], Lon: anchor[0], Area: geo.Area(region), LastChanged: parts[0].changed, Geometry: raw, region: region, parts: parts}
			for _, p := range parts {
				candidate.Spawnpoints += p.count
			}
			encoded, err := json.Marshal(candidate)
			if err != nil {
				return nil, err
			}
			candidate.encoded = encoded
			candidate.digest = sha256.Sum256(encoded)
			entry = candidate
		}
		for _, p := range parts {
			for _, alias := range p.aliases {
				s.aliases[strconv.FormatInt(alias, 10)] = key
			}
		}
		s.entries[key] = entry
		b := entry.region.Bound()
		s.tree.Insert(b.Min, b.Max, entry)
		h.Write(entry.digest[:])
		counts[parts[0].pair]++
	}
	// Alias changes also alter direct-ID responses and must change the revision.
	var aliases []string
	for id := range s.aliases {
		aliases = append(aliases, id)
	}
	sort.Strings(aliases)
	for _, id := range aliases {
		fmt.Fprintf(h, "%s:%s;", id, s.aliases[id])
	}
	for pair, count := range counts {
		s.stats = append(s.stats, mapStat{pair.Pokemon, pair.Form, count})
	}
	sort.Slice(s.stats, func(i, j int) bool {
		if s.stats[i].Pokemon != s.stats[j].Pokemon {
			return s.stats[i].Pokemon < s.stats[j].Pokemon
		}
		return s.stats[i].Form < s.stats[j].Form
	})
	s.revision = hex.EncodeToString(h.Sum(nil))
	return s, nil
}
