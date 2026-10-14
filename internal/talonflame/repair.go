package talonflame

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/golang/geo/s2"
	"github.com/sirupsen/logrus"
)

// Repair separates coherent alternate populations from varied, non-nesting
// points. It is deliberately paused during the early-hour change window and
// requires fresh backing for the parent's species before rejecting its points.
func (a *Analyzer) Repair(ctx context.Context, now time.Time) error {
	points := make([]uint64, 0, len(a.byPoint))
	for id := range a.byPoint {
		points = append(points, id)
	}
	sort.Slice(points, func(i, j int) bool { return points[i] < points[j] })
	for _, id := range points {
		state, err := a.point(ctx, id)
		if err != nil {
			return err
		}
		if state.Status == "outside" || state.Status == "invalid" {
			continue
		}
		group := a.groups[state.Nest]
		if group != nil {
			if !group.Confirmed || now.Unix() < hourBoundary(now, group.zone)+int64(a.cfg.ChangeWindow/time.Second) {
				continue
			}
			backed := false
			for _, o := range a.byGroup[group.ID] {
				if o.Spawnpoint != id && o.Pair == group.Pair && o.At >= hourBoundary(now, group.zone) {
					backed = true
					break
				}
			}
			if !backed {
				continue
			}
		}
		observations := make([]Observation, 0, len(a.byPoint[id]))
		for _, o := range a.byPoint[id] {
			observations = append(observations, o)
		}
		sort.Slice(observations, func(i, j int) bool { return observations[i].At > observations[j].At })
		seen := map[uint64]bool{}
		pairs := map[Pair]int{}
		hours := map[int64]bool{}
		var streak []Observation
		for _, o := range observations {
			if o.At < now.Add(-a.cfg.Retention).Unix() {
				break
			}
			if group != nil && group.LastChanged != nil && o.At < *group.LastChanged {
				break
			}
			if group != nil && o.Pair == group.Pair {
				break
			}
			if seen[o.Encounter] {
				continue
			}
			seen[o.Encounter] = true
			streak = append(streak, o)
			pairs[o.Pair]++
			hours[o.Hour] = true
		}
		if len(streak) < a.cfg.MismatchMinimum {
			continue
		}
		if len(pairs) > 1 {
			if state.Nest != 0 {
				if err := a.assign(ctx, id, 0, "rejected", "repeated varied-species low-roll contradictions", nil); err != nil {
					return err
				}
				a.logger.WithFields(logrus.Fields{"spawnpoint_id": fmt.Sprintf("%x", id), "old_nest_id": state.Nest, "contradictions": len(streak), "distinct_species": len(pairs)}).Info("spawnpoint removed from nest")
			}
			continue
		}
		// Consistency across independent spawn cycles is a subnest signal, not
		// evidence for deletion. Hour diversity also guards against payload spam.
		if len(hours) < 2 {
			continue
		}
		pair := streak[0].Pair
		_, sources := a.membership.Match(id)
		if len(sources) == 0 {
			continue
		}
		target := a.alternate(id, sources, pair, state.Nest)
		created := false
		if target == 0 {
			// Fully overlapping polygons may have no unambiguous initial
			// members. Bootstrap a provisional coherent group instead of waiting
			// forever for one of those empty imported groups to confirm first.
			target, err = a.createGroup(ctx, sources, pair, group)
			if err != nil {
				return err
			}
			created = true
		}
		if err := a.assign(ctx, id, target, "learned", "consistent alternate low-roll species", &pair); err != nil {
			return err
		}
		event := "spawnpoint reassigned"
		if created {
			event = "provisional subnest discovered"
		}
		a.logger.WithFields(logrus.Fields{"spawnpoint_id": fmt.Sprintf("%x", id), "old_nest_id": state.Nest, "nest_id": target, "species": pair.String(), "support": len(streak)}).Info(event)
	}
	return a.Evaluate(ctx, now)
}

func (a *Analyzer) alternate(point uint64, sources []int64, pair Pair, exclude int64) int64 {
	var best int64
	distance := a.cfg.ClusterRadius
	center := s2.CellID(point << 20).LatLng()
	for _, source := range sources {
		for _, id := range a.sources[source] {
			if id == exclude {
				continue
			}
			g := a.groups[id]
			if g == nil || g.Pair != pair {
				continue
			}
			for _, o := range a.byGroup[id] {
				if o.Pair != pair {
					continue
				}
				d := float64(center.Distance(s2.CellID(o.Spawnpoint<<20).LatLng())) * 6371000
				if d < distance || (d == distance && (best == 0 || id < best)) {
					best, distance = id, d
				}
			}
		}
	}
	return best
}

func (a *Analyzer) createGroup(ctx context.Context, sources []int64, pair Pair, parent *Group) (int64, error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	zone := ""
	location := time.UTC
	if parent != nil {
		zone, location = parent.Timezone, parent.zone
	}
	var id int64
	if err := tx.QueryRowContext(ctx, "SELECT LEAST(COALESCE(MIN(nest_id),0),0)-1 FROM talonflame_nest_groups").Scan(&id); err != nil {
		return 0, err
	}
	// The database-wide writer lease serializes ID allocation with imports.
	if _, err = tx.ExecContext(ctx, "INSERT INTO talonflame_nest_groups (nest_id,pokemon_id,pokemon_form,status,timezone) VALUES (?,?,?,'provisional',?)", id, pair.Pokemon, pair.Form, zone); err != nil {
		return 0, err
	}
	for _, source := range sources {
		if _, err = tx.ExecContext(ctx, "INSERT INTO talonflame_nest_sources (group_id,source_nest_id) VALUES (?,?)", id, source); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE nests SET pokemon_id=NULL,pokemon_form=NULL,last_changed=NULL WHERE nest_id=?", source); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	a.groups[id] = &Group{ID: id, Pair: pair, Timezone: zone, zone: location}
	for _, source := range sources {
		a.sources[source] = append(a.sources[source], id)
	}
	a.groupSources[id] = sources
	return id, nil
}

func (a *Analyzer) assign(ctx context.Context, point uint64, nest int64, status, reason string, pair *Pair) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE talonflame_spawnpoint_state SET nest_id=?,status=?,reason=? WHERE spawnpoint_id=?", nullableNest(nest), status, reason, point); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE spawnpoint SET talonflame_nest_id=? WHERE id=?", nullableNest(nest), point); err != nil {
		return err
	}
	// Move only evidence for the coherent alternate pair. For rejected points,
	// remove all retained votes from the former group's consensus.
	query := "UPDATE talonflame_evidence SET nest_id=? WHERE spawnpoint_id=?"
	args := []any{nullableNest(nest), point}
	if pair != nil {
		query += " AND pokemon_id=? AND pokemon_form=?"
		args = append(args, pair.Pokemon, pair.Form)
	}
	if _, err = tx.ExecContext(ctx, query, args...); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	a.points[point] = pointState{Nest: nest, Status: status}
	for _, o := range a.byPoint[point] {
		if pair == nil || o.Pair == *pair {
			o.Nest = nest
			a.remember(o)
		}
	}
	return nil
}

func (a *Analyzer) Maintain(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-a.cfg.Retention).Unix()
	if _, err := a.db.ExecContext(ctx, "DELETE FROM talonflame_evidence WHERE observed_at<?", cutoff); err != nil {
		return err
	}
	for key, o := range a.evidence {
		if o.At < cutoff {
			delete(a.evidence, key)
			delete(a.byGroup[o.Nest], key)
			delete(a.byPoint[o.Spawnpoint], key)
			if len(a.byPoint[o.Spawnpoint]) == 0 {
				delete(a.byPoint, o.Spawnpoint)
				delete(a.points, o.Spawnpoint)
			}
			if len(a.byGroup[o.Nest]) == 0 {
				delete(a.byGroup, o.Nest)
			}
		}
	}
	for id, state := range a.points {
		if state.Status == "outside" || state.Status == "invalid" {
			delete(a.points, id)
		}
	}
	// Golbat may create a DB row after sending its first webhook. Reconcile
	// that late row without needing SELECT on Talonflame's spawnpoint column.
	_, err := a.db.ExecContext(ctx, `UPDATE spawnpoint p JOIN talonflame_spawnpoint_state s ON s.spawnpoint_id=p.id
	 SET p.talonflame_nest_id=s.nest_id`)
	return err
}
