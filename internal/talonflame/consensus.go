package talonflame

import "time"

type decision struct {
	Pair           Pair
	Boundary       int64
	Support, Total int
}

func consensus(observations []Observation, minimum int, agreement float64) (Pair, int, bool) {
	counts := map[Pair]int{}
	var best Pair
	for _, observation := range observations {
		counts[observation.Pair]++
		if counts[observation.Pair] > counts[best] || (counts[observation.Pair] == counts[best] && (observation.Pair.Pokemon < best.Pokemon || (observation.Pair.Pokemon == best.Pokemon && observation.Pair.Form < best.Form))) {
			best = observation.Pair
		}
	}
	return best, counts[best], counts[best] >= minimum && float64(counts[best]) >= agreement*float64(len(observations))
}

// A low roll is the ordinary weighted spawn-table coordinate as well as the
// nest gate. An ordinary species can occupy a contiguous slice of that gate.
// Agreement alone therefore cannot establish a nest: supporting rolls must
// reach both the bottom and top quarters of the low-roll interval.
func coversNestGate(observations []Observation, pair Pair) bool {
	var lower, upper bool
	for _, observation := range observations {
		if observation.Pair != pair || observation.Roll >= 1<<22 {
			continue
		}
		lower = lower || observation.Roll < 1<<20
		upper = upper || observation.Roll >= 3<<20
	}
	return lower && upper
}

func nestConsensus(observations []Observation, minimum int, agreement float64) (Pair, int, bool) {
	// Reobserving a surviving encounter in a later hour is not another
	// independent confirmation. Keep its latest payload in a challenger run.
	type identity struct{ encounter, spawnpoint uint64 }
	latest := map[identity]Observation{}
	for _, observation := range observations {
		key := identity{observation.Encounter, observation.Spawnpoint}
		old, exists := latest[key]
		if !exists || observation.At > old.At || (observation.At == old.At && observation.Hour > old.Hour) {
			latest[key] = observation
		}
	}
	rows := make([]Observation, 0, len(latest))
	for _, observation := range latest {
		rows = append(rows, observation)
	}
	pair, support, ok := consensus(rows, minimum, agreement)
	return pair, support, ok && coversNestGate(rows, pair)
}

func infer(group *Group, observations []Observation, now time.Time, minimum int, agreement float64) (decision, bool) {
	boundary := hourBoundary(now, group.zone)
	var current []Observation
	for _, observation := range observations {
		if observation.At >= boundary && observation.At <= now.Unix() {
			current = append(current, observation)
		}
	}
	if pair, support, ok := nestConsensus(current, minimum, agreement); ok {
		return decision{pair, boundary, support, len(current)}, true
	}
	// Migrations occur at an hour boundary. Do not turn scan-order effects
	// inside a mixed polygon into a migration just because five observations
	// of a rival population arrived after the latest supporting old-species
	// observation in the SAME hour. The cross-hour fallback is only for a
	// challenger run whose old species has not also been seen this hour.
	if group.Confirmed {
		for _, observation := range current {
			if observation.Pair == group.Pair {
				return decision{}, false
			}
		}
	}
	// Sparse/single-spawnpoint nests can accumulate confirmations across hours.
	// A matching observation of the old label interrupts the challenger run.
	var lastOld int64
	if group.Confirmed {
		for _, observation := range observations {
			if observation.Pair == group.Pair && observation.At > lastOld {
				lastOld = observation.At
			}
		}
		if group.LastChanged != nil && *group.LastChanged > lastOld {
			lastOld = *group.LastChanged
		}
	}
	var candidates []Observation
	first := now.Unix()
	for _, observation := range observations {
		if observation.At > lastOld && observation.At <= now.Unix() {
			candidates = append(candidates, observation)
			if observation.At < first {
				first = observation.At
			}
		}
	}
	pair, support, ok := nestConsensus(candidates, minimum, agreement)
	return decision{pair, hourBoundary(time.Unix(first, 0), group.zone), support, len(candidates)}, ok
}

func conflictingEvidence(group *Group, observations []Observation, now time.Time, minimum int, agreement float64) map[Pair]int {
	boundary := hourBoundary(now, group.zone)
	counts := map[Pair]int{}
	total := 0
	for _, observation := range observations {
		if observation.At >= boundary && observation.At <= now.Unix() {
			counts[observation.Pair]++
			total++
		}
	}
	// Confirmation must remain subject to the configured agreement threshold.
	// Requiring five votes for ONE rival left confirmations sticky when the
	// contradictions were distributed among several ordinary species.
	// No supporting observation this hour may instead mean an actual migration;
	// carry the old label while the challenger accumulates sufficient evidence.
	if total >= minimum && counts[group.Pair] > 0 && float64(counts[group.Pair]) < agreement*float64(total) {
		return counts
	}
	return nil
}

func latestPointConsensus(observations []Observation, minimum int, agreement float64) (Pair, bool) {
	latest := map[uint64]Observation{}
	for _, observation := range observations {
		prior, ok := latest[observation.Spawnpoint]
		if !ok || observation.At > prior.At || (observation.At == prior.At && observation.Encounter > prior.Encounter) {
			latest[observation.Spawnpoint] = observation
		}
	}
	rows := make([]Observation, 0, len(latest))
	for _, observation := range latest {
		rows = append(rows, observation)
	}
	pair, _, ok := consensus(rows, minimum, agreement)
	return pair, ok
}
