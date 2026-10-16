package talonflame

import (
	"runtime"
	"testing"
)

func newEvidenceCache() *Analyzer {
	return &Analyzer{points: map[uint64]pointState{}, evidence: map[evidenceKey]*Observation{}, byPoint: map[uint64][]*Observation{}, byGroup: map[int64]map[*Observation]struct{}{}, dirty: map[int64]bool{}}
}

func TestEvidenceRefreshReassignmentAndExpiry(t *testing.T) {
	a := newEvidenceCache()
	old := Observation{Encounter: 1, Spawnpoint: 11, Nest: 21, Pair: Pair{25, 0}, Hour: 3600, At: 3700}
	a.remember(old)
	refreshed := old
	refreshed.At = 3900
	refreshed.Pair = Pair{54, 0}
	a.remember(refreshed)
	if len(a.evidence) != 1 || len(a.byPoint[11]) != 1 || len(a.byGroup[21]) != 1 {
		t.Fatal("refresh duplicated an independent observation")
	}
	if got := *a.byPoint[11][0]; got != refreshed {
		t.Fatal("point index kept a stale observation", got)
	}
	clear(a.dirty)
	refreshed.Nest = -1
	a.remember(refreshed)
	if len(a.byGroup[21]) != 0 || len(a.byGroup[-1]) != 1 || !a.dirty[21] || !a.dirty[-1] {
		t.Fatal("reassignment did not move the vote and invalidate both groups")
	}
	// The same identity in another hour remains separately stored for migration
	// witnesses, with independent expiry of each hour's most recent observation.
	later := refreshed
	later.Hour, later.At = 7200, 7300
	a.remember(later)
	a.points[11] = pointState{Nest: -1, Status: "learned"}
	a.expireEvidence(3900)
	if len(a.evidence) != 2 {
		t.Fatal("expired the cutoff itself or ignored the refreshed timestamp")
	}
	a.expireEvidence(3901)
	if len(a.evidence) != 1 || len(a.byPoint[11]) != 1 || len(a.byGroup[-1]) != 1 || *a.byPoint[11][0] != later {
		t.Fatal("expiry removed a live vote or left an expired vote in an index")
	}
	for _, entry := range a.byPoint[11][len(a.byPoint[11]):cap(a.byPoint[11])] {
		if entry != nil {
			t.Fatal("unused slice tail retained expired evidence")
		}
	}
	a.expireEvidence(7301)
	if len(a.evidence) != 0 || len(a.byPoint) != 0 || len(a.byGroup) != 0 || len(a.points) != 0 {
		t.Fatal("expiry retained empty indexes or unused point state")
	}
}

func TestNegativePointCacheBounded(t *testing.T) {
	a := newEvidenceCache()
	for id := uint64(0); id < 3*negativePointCacheLimit; id++ {
		a.cacheNegativePoint(id, "outside")
		if len(a.negativePoints) > negativePointCacheLimit {
			t.Fatal("negative point cache exceeded its bound")
		}
	}
	a.cacheNegativePoint(1, "invalid")
	if !a.negativePoints[1] || len(a.points) != 0 {
		t.Fatal("negative point polluted persistent membership state")
	}
}

func BenchmarkEvidenceCache(b *testing.B) {
	const count = 200000
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		a := newEvidenceCache()
		for i := 0; i < count; i++ {
			a.remember(Observation{Encounter: uint64(i + 1), Spawnpoint: uint64(i/4 + 1), Nest: int64(i/400 + 1), Pair: Pair{25, 0}, Roll: uint32(i % (1 << 22)), At: int64(1700000000 + i), Hour: 1700000000})
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc)/count, "retained-B/evidence")
		runtime.KeepAlive(a)
	}
}
