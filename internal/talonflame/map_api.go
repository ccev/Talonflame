package talonflame

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/paulmach/orb"
)

type mapResponse struct {
	Revision     string            `json:"revision"`
	Snapshot     bool              `json:"snapshot"`
	Examined     int               `json:"examined"`
	LimitReached bool              `json:"limitReached,omitempty"`
	Data         []json.RawMessage `json:"data"`
}

// Requests only read the published immutable spatial index. They never access
// SQL, the analyzer's evidence, or the polygon splitting/union code.
func (m *mapStore) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "GET required", 405)
		return
	}
	snapshot := m.current.Load()
	if snapshot == nil || time.Now().Unix()-m.lastSuccess.Load() > 120 {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "map snapshot unavailable", 503)
		return
	}
	q := r.URL.Query()
	limit := 10000
	if value := q.Get("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 10000 {
			http.Error(w, "limit must be 1..10000", 400)
			return
		}
		limit = n
	}
	var boxes []orb.Bound
	id := q.Get("id")
	if id != "" {
		if n, err := strconv.ParseInt(id, 10, 64); err != nil || n == 0 {
			http.Error(w, "invalid id", 400)
			return
		}
	} else if r.URL.Path != "/api/map/nests/stats" {
		values := strings.Split(q.Get("bbox"), ",")
		if len(values) != 4 {
			http.Error(w, "bbox=west,south,east,north required", 400)
			return
		}
		var b [4]float64
		for i, value := range values {
			n, err := strconv.ParseFloat(value, 64)
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
				http.Error(w, "invalid bbox", 400)
				return
			}
			b[i] = n
		}
		if b[0] < -180 || b[0] > 180 || b[2] < -180 || b[2] > 180 || b[1] < -90 || b[3] > 90 || b[1] >= b[3] {
			http.Error(w, "invalid bbox", 400)
			return
		}
		if b[0] <= b[2] {
			boxes = []orb.Bound{{Min: orb.Point{b[0], b[1]}, Max: orb.Point{b[2], b[3]}}}
		} else {
			boxes = []orb.Bound{{Min: orb.Point{b[0], b[1]}, Max: orb.Point{180, b[3]}}, {Min: orb.Point{-180, b[1]}, Max: orb.Point{b[2], b[3]}}}
		}
	}
	pokemon := map[int]bool{}
	if value := q.Get("pokemon"); value != "" {
		parts := strings.Split(value, ",")
		if len(parts) > 2000 {
			http.Error(w, "too many species", 400)
			return
		}
		for _, part := range parts {
			n, err := strconv.Atoi(part)
			if err != nil || n < 1 {
				http.Error(w, "invalid pokemon", 400)
				return
			}
			pokemon[n] = true
		}
	}
	h := sha256.Sum256([]byte(snapshot.revision + ":" + r.URL.Path + "?" + q.Encode()))
	etag := `"` + hex.EncodeToString(h[:]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Content-Type", "application/json")
	for _, tag := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		if strings.TrimSpace(tag) == etag || strings.TrimSpace(tag) == "*" {
			w.WriteHeader(304)
			return
		}
	}
	if r.URL.Path == "/api/map/nests/stats" {
		json.NewEncoder(w).Encode(struct {
			Revision string    `json:"revision"`
			Data     []mapStat `json:"data"`
		}{snapshot.revision, snapshot.stats})
		return
	}
	response := mapResponse{Revision: snapshot.revision, Snapshot: true, Data: []json.RawMessage{}}
	var matches []*MapNest
	if id != "" {
		key := id
		if alias, ok := snapshot.aliases[id]; ok {
			key = alias
		}
		if entry := snapshot.entries[key]; entry != nil {
			copy := *entry
			copy.ID = id
			raw, _ := json.Marshal(&copy)
			copy.encoded = raw
			matches = append(matches, &copy)
		}
	} else {
		seen := map[string]bool{}
		size := 0
		for _, box := range boxes {
			snapshot.tree.Search(box.Min, box.Max, func(min, max [2]float64, entry *MapNest) bool {
				if seen[entry.ID] || (len(pokemon) > 0 && !pokemon[entry.Pokemon]) {
					return true
				}
				seen[entry.ID] = true
				matches = append(matches, entry)
				size += len(entry.encoded)
				if len(matches) > limit || size > 16<<20 {
					response.LimitReached = true
					return false
				}
				return true
			})
			if response.LimitReached {
				break
			}
		}
	}
	if response.LimitReached {
		response.Examined = limit
	} else {
		sort.Slice(matches, func(i, j int) bool { return matches[i].ID < matches[j].ID })
		for _, entry := range matches {
			response.Data = append(response.Data, entry.encoded)
		}
		response.Examined = len(matches)
	}
	json.NewEncoder(w).Encode(response)
}
