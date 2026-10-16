package talonflame

// Keep one canonical observation. Group indexes contain references, and point
// indexes use small slices rather than allocating a hash table per spawnpoint.
// At the live feed's rate this accommodates roughly an hour's outside points
// while retaining only an ID and an invalid flag, rather than full point state.
const negativePointCacheLimit = 1 << 20

func (a *Analyzer) cacheNegativePoint(id uint64, status string) {
	if a.negativePoints == nil || len(a.negativePoints) >= negativePointCacheLimit {
		a.negativePoints = make(map[uint64]bool)
	}
	a.negativePoints[id] = status == "invalid"
}

func (a *Analyzer) expireEvidence(cutoff int64) {
	for point, entries := range a.byPoint {
		live := entries[:0]
		for _, entry := range entries {
			if entry.At >= cutoff {
				live = append(live, entry)
				continue
			}
			delete(a.evidence, entry.key())
			delete(a.byGroup[entry.Nest], entry)
			if len(a.byGroup[entry.Nest]) == 0 {
				delete(a.byGroup, entry.Nest)
			}
			a.dirty[entry.Nest] = true
		}
		// Release references in the unused tail of the backing array as well.
		for i := len(live); i < len(entries); i++ {
			entries[i] = nil
		}
		if len(live) == 0 {
			delete(a.byPoint, point)
			delete(a.points, point)
		} else {
			if cap(live) > 8 && cap(live) > 2*len(live) {
				live = append([]*Observation(nil), live...)
			}
			a.byPoint[point] = live
		}
	}
}
