package talonflame

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
	_ "time/tzdata"

	"github.com/UnownHash/Fletchling/internal/talonmigration"
	"github.com/sirupsen/logrus"
)

type Config struct {
	Minimum         int
	Agreement       float64
	MismatchMinimum int
	ChangeWindow    time.Duration
	Retention       time.Duration
	ClusterRadius   float64
}

func DefaultConfig() Config {
	return Config{Minimum: 5, Agreement: .9, MismatchMinimum: 3, ChangeWindow: 10 * time.Minute, Retention: 48 * time.Hour, ClusterRadius: 250}
}

func (cfg Config) Validate() error {
	if cfg.Minimum < 2 || cfg.Agreement <= .5 || cfg.Agreement > 1 || cfg.MismatchMinimum < 3 || cfg.Retention < time.Hour || cfg.ChangeWindow < 0 || cfg.ChangeWindow >= time.Hour || cfg.ClusterRadius <= 0 {
		return fmt.Errorf("invalid analyzer thresholds")
	}
	return nil
}

type Analyzer struct {
	db                *sql.DB
	lock              *sql.Conn
	logger            *logrus.Logger
	cfg               Config
	membership        *talonmigration.Membership
	groups            map[int64]*Group
	sources           map[int64][]int64
	groupSources      map[int64][]int64
	points            map[uint64]pointState
	evidence          map[evidenceKey]Observation
	byGroup           map[int64]map[evidenceKey]Observation
	byPoint           map[uint64]map[evidenceKey]Observation
	dirty             map[int64]bool
	knownGroups       uint64
	conflictingGroups uint64
}

func New(ctx context.Context, db *sql.DB, logger *logrus.Logger, cfg Config) (*Analyzer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	a := &Analyzer{db: db, logger: logger, cfg: cfg, groups: map[int64]*Group{}, sources: map[int64][]int64{}, groupSources: map[int64][]int64{}, points: map[uint64]pointState{}, evidence: map[evidenceKey]Observation{}, byGroup: map[int64]map[evidenceKey]Observation{}, byPoint: map[uint64]map[evidenceKey]Observation{}, dirty: map[int64]bool{}}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	a.lock = conn
	var locked sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(CONCAT(DATABASE(), ':talonflame-writer'), 0)").Scan(&locked); err != nil {
		conn.Close()
		return nil, err
	}
	if !locked.Valid || locked.Int64 != 1 {
		conn.Close()
		return nil, fmt.Errorf("another Talonflame writer is running")
	}
	ok := false
	defer func() {
		if !ok {
			a.Close()
		}
	}()
	// A read-only checksum validation ensures this binary matches the applied
	// migrations. Schema application is always a separate explicit command.
	if err := talonmigration.Run(ctx, conn, logger, talonmigration.Options{Mode: "schema", BatchSize: 1000}); err != nil {
		return nil, err
	}
	var applied int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM talonflame_schema_migrations WHERE version='001_schema.sql'").Scan(&applied); err != nil || applied != 1 {
		return nil, fmt.Errorf("apply Talonflame schema first")
	}
	a.membership, err = talonmigration.LoadMembership(ctx, conn)
	if err != nil {
		return nil, err
	}
	if err := a.restore(ctx); err != nil {
		return nil, err
	}
	ok = true
	logger.WithFields(logrus.Fields{"groups": len(a.groups), "evidence": len(a.evidence)}).Info("Talonflame analyzer ready")
	return a, nil
}

func (a *Analyzer) Close() {
	if a.lock == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a.lock.ExecContext(ctx, "DO RELEASE_LOCK(CONCAT(DATABASE(), ':talonflame-writer'))")
	a.lock.Close()
	a.lock = nil
}

func (a *Analyzer) restore(ctx context.Context) error {
	rows, err := a.db.QueryContext(ctx, "SELECT nest_id,pokemon_id,pokemon_form,last_changed,status,timezone FROM talonflame_nest_groups")
	if err != nil {
		return err
	}
	for rows.Next() {
		var g Group
		var pokemon, form, changed sql.NullInt64
		var timezone sql.NullString
		var status string
		if err := rows.Scan(&g.ID, &pokemon, &form, &changed, &status, &timezone); err != nil {
			rows.Close()
			return err
		}
		g.Pair = Pair{int(pokemon.Int64), int(form.Int64)}
		g.Confirmed = (status == "confirmed" || status == "conflicting") && pokemon.Valid && form.Valid
		g.Conflicting = status == "conflicting" && g.Confirmed
		if g.Confirmed {
			a.knownGroups++
		}
		if g.Conflicting {
			a.conflictingGroups++
		}
		if changed.Valid {
			value := changed.Int64
			g.LastChanged = &value
		}
		g.Timezone = timezone.String
		g.zone = time.UTC
		if g.Timezone != "" {
			g.zone, err = time.LoadLocation(g.Timezone)
			if err != nil {
				rows.Close()
				return fmt.Errorf("nest %d timezone: %w", g.ID, err)
			}
		}
		a.groups[g.ID] = &g
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = a.db.QueryContext(ctx, "SELECT group_id,source_nest_id FROM talonflame_nest_sources")
	if err != nil {
		return err
	}
	for rows.Next() {
		var group, source int64
		if err := rows.Scan(&group, &source); err != nil {
			rows.Close()
			return err
		}
		a.sources[source] = append(a.sources[source], group)
		a.groupSources[group] = append(a.groupSources[group], source)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = a.db.QueryContext(ctx, `SELECT hour_start,encounter_id,spawnpoint_id,nest_id,pokemon_id,pokemon_form,roll_u24,observed_at
	 FROM talonflame_evidence WHERE observed_at>=?`, time.Now().Add(-a.cfg.Retention).Unix())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var o Observation
		var nest, form sql.NullInt64
		if err := rows.Scan(&o.Hour, &o.Encounter, &o.Spawnpoint, &nest, &o.Pair.Pokemon, &form, &o.Roll, &o.At); err != nil {
			return err
		}
		if !form.Valid {
			continue
		}
		o.Pair.Form = int(form.Int64)
		o.Nest = nest.Int64
		a.remember(o)
	}
	return rows.Err()
}

func (a *Analyzer) remember(o Observation) {
	key := o.key()
	if old, ok := a.evidence[key]; ok && old.Nest != o.Nest {
		delete(a.byGroup[old.Nest], key)
		a.dirty[old.Nest] = true
	}
	a.evidence[key] = o
	if a.byPoint[o.Spawnpoint] == nil {
		a.byPoint[o.Spawnpoint] = map[evidenceKey]Observation{}
	}
	a.byPoint[o.Spawnpoint][key] = o
	if a.byGroup[o.Nest] == nil {
		a.byGroup[o.Nest] = map[evidenceKey]Observation{}
	}
	a.byGroup[o.Nest][key] = o
	a.dirty[o.Nest] = true
}

func (a *Analyzer) Process(ctx context.Context, pokemon Pokemon, now time.Time) error {
	o, eligible, err := parsePokemon(pokemon, now)
	if err != nil {
		return err
	}
	if !eligible {
		return nil
	}
	if o.At < now.Add(-a.cfg.Retention).Unix() {
		return nil
	}
	state, err := a.point(ctx, o.Spawnpoint)
	if err != nil {
		return err
	}
	if state.Status == "outside" || state.Status == "invalid" {
		return nil
	}
	o.Nest = state.Nest
	zone := time.UTC
	if group := a.groups[o.Nest]; group != nil {
		zone = group.zone
	}
	o.Hour = hourBoundary(time.Unix(o.At, 0), zone)
	old, exists := a.evidence[o.key()]
	duplicate := exists && old.Pair == o.Pair
	if a.logger.IsLevelEnabled(logrus.DebugLevel) {
		a.logger.WithFields(logrus.Fields{
			"encounter_id": fmt.Sprint(o.Encounter), "spawnpoint_id": fmt.Sprintf("%x", o.Spawnpoint), "nest_id": o.Nest,
			"pokemon_id": o.Pair.Pokemon, "form": o.Pair.Form, "roll_u24": o.Roll, "roll": float64(o.Roll) / (1 << 24), "duplicate": duplicate,
		}).Debug("low-roll encounter")
	}
	if exists && (o.At < old.At || (o.At == old.At && duplicate)) {
		return nil
	}
	_, err = a.db.ExecContext(ctx, `INSERT INTO talonflame_evidence
	 (hour_start,encounter_id,spawnpoint_id,nest_id,pokemon_id,pokemon_form,roll_u24,observed_at) VALUES (?,?,?,?,?,?,?,?)
	 ON DUPLICATE KEY UPDATE nest_id=VALUES(nest_id),pokemon_id=VALUES(pokemon_id),pokemon_form=VALUES(pokemon_form),
	 roll_u24=VALUES(roll_u24),observed_at=VALUES(observed_at)`, o.Hour, o.Encounter, o.Spawnpoint, nullableNest(o.Nest), o.Pair.Pokemon, o.Pair.Form, o.Roll, o.At)
	if err != nil {
		return err
	}
	a.remember(o)
	return nil
}

func nullableNest(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func (a *Analyzer) point(ctx context.Context, id uint64) (pointState, error) {
	if state, ok := a.points[id]; ok {
		return state, nil
	}
	var nest sql.NullInt64
	var state pointState
	err := a.db.QueryRowContext(ctx, "SELECT nest_id,status FROM talonflame_spawnpoint_state WHERE spawnpoint_id=?", id).Scan(&nest, &state.Status)
	if err == nil {
		state.Nest = nest.Int64
		a.points[id] = state
		return state, nil
	}
	if err != sql.ErrNoRows {
		return state, err
	}
	status, candidates := a.membership.Match(id)
	groupSet := map[int64]bool{}
	for _, source := range candidates {
		for _, group := range a.sources[source] {
			groupSet[group] = true
		}
	}
	state.Status = status
	if status == "outside" || status == "invalid" {
		a.points[id] = state
		return state, nil
	}
	if len(candidates) > 0 {
		state.Status = "ambiguous"
		if len(groupSet) == 1 {
			for id := range groupSet {
				state.Nest = id
			}
			state.Status = "imported"
		}
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return state, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO talonflame_spawnpoint_state (spawnpoint_id,nest_id,status) VALUES (?,?,?)", id, nullableNest(state.Nest), state.Status); err != nil {
		return state, err
	}
	for _, source := range candidates {
		if _, err = tx.ExecContext(ctx, "INSERT INTO talonflame_spawnpoint_candidates (spawnpoint_id,source_nest_id) VALUES (?,?)", id, source); err != nil {
			return state, err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE spawnpoint SET talonflame_nest_id=? WHERE id=?", nullableNest(state.Nest), id); err != nil {
		return state, err
	}
	if err = tx.Commit(); err != nil {
		return state, err
	}
	a.points[id] = state
	return state, nil
}

func (a *Analyzer) Evaluate(ctx context.Context, now time.Time) error {
	ids := make([]int64, 0, len(a.dirty))
	for id := range a.dirty {
		if id != 0 {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		group := a.groups[id]
		if group == nil {
			delete(a.dirty, id)
			continue
		}
		observations := make([]Observation, 0, len(a.byGroup[id]))
		for _, o := range a.byGroup[id] {
			if o.At >= now.Add(-a.cfg.Retention).Unix() {
				observations = append(observations, o)
			}
		}
		if group.Confirmed && !group.Conflicting {
			var backing []Observation
			for _, observation := range observations {
				if observation.Pair == group.Pair && observation.At <= now.Unix() && (group.LastChanged == nil || observation.At >= *group.LastChanged) {
					backing = append(backing, observation)
				}
			}
			// Revalidate labels established by the older agreement-only detector.
			// Preserve carried labels if retained evidence is sparse or expired.
			if len(backing) >= a.cfg.Minimum && !coversNestGate(backing, group.Pair) {
				if err := a.withdraw(ctx, id, "provisional"); err != nil {
					return err
				}
				group.Confirmed = false
				a.knownGroups--
				a.logger.WithFields(logrus.Fields{"nest_id": id, "species": group.Pair.String(), "support": len(backing)}).Info("nest confirmation withdrawn: insufficient roll coverage")
			}
		}
		d, ok := infer(group, observations, now, a.cfg.Minimum, a.cfg.Agreement)
		if ok && group.Conflicting {
			// A previously mixed polygon must not become a single-species nest
			// merely because the scanner sampled one corner first this hour.
			// Require the latest evidence across its known points to agree too.
			pair, consistent := latestPointConsensus(observations, a.cfg.Minimum, a.cfg.Agreement)
			ok = consistent && pair == d.Pair
		}
		if ok && (!group.Confirmed || group.Pair != d.Pair || group.Conflicting) {
			changed := group.LastChanged
			inferredChange := false
			if group.Confirmed && group.Pair != d.Pair {
				// Revising an initial label within its first observed hour is a
				// baseline correction, not evidence that a migration happened at
				// the beginning of that hour. Require an established earlier period.
				inferredChange = group.LastChanged != nil && *group.LastChanged < d.Boundary
				for _, observation := range observations {
					if observation.Pair == group.Pair && observation.At < d.Boundary && (group.LastChanged == nil || observation.At >= *group.LastChanged) {
						inferredChange = true
						break
					}
				}
				if inferredChange {
					value := d.Boundary
					changed = &value
				}
			}
			tx, err := a.db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE talonflame_nest_groups SET pokemon_id=?,pokemon_form=?,last_changed=?,status='confirmed' WHERE nest_id=?", d.Pair.Pokemon, d.Pair.Form, changed, id); err == nil {
				err = a.project(ctx, tx, id, d.Pair, changed)
			}
			if err != nil {
				tx.Rollback()
				return err
			}
			if err = tx.Commit(); err != nil {
				return err
			}
			event := "nest confirmed"
			if group.Confirmed {
				if group.Pair != d.Pair {
					if inferredChange {
						event = "nest changed"
					} else {
						event = "nest baseline revised"
					}
				} else {
					event = "nest conflict resolved"
				}
			}
			var loggedBoundary any
			if changed != nil {
				loggedBoundary = *changed
			}
			a.logger.WithFields(logrus.Fields{"nest_id": id, "old_species": group.Pair.String(), "species": d.Pair.String(), "last_changed": loggedBoundary, "support": d.Support, "total": d.Total}).Info(event)
			if !group.Confirmed {
				a.knownGroups++
			}
			if group.Conflicting {
				a.conflictingGroups--
			}
			group.Pair, group.Confirmed, group.LastChanged = d.Pair, true, changed
			group.Conflicting = false
		} else if !ok && group.Confirmed && !group.Conflicting {
			if counts := conflictingEvidence(group, observations, now, a.cfg.Minimum, a.cfg.Agreement); counts != nil {
				if err := a.withdraw(ctx, id, "conflicting"); err != nil {
					return err
				}
				group.Conflicting = true
				a.conflictingGroups++
				votes := map[string]int{}
				for pair, count := range counts {
					votes[pair.String()] = count
				}
				a.logger.WithFields(logrus.Fields{"nest_id": id, "species": group.Pair.String(), "votes": votes}).Info("nest has conflicting low-roll populations")
			}
		}
		delete(a.dirty, id)
	}
	return nil
}

func (a *Analyzer) withdraw(ctx context.Context, id int64, status string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE talonflame_nest_groups SET status=? WHERE nest_id=?", status, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE nests n JOIN talonflame_nest_sources s ON s.source_nest_id=n.nest_id
	 SET n.pokemon_id=NULL,n.pokemon_form=NULL WHERE s.group_id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *Analyzer) project(ctx context.Context, tx *sql.Tx, id int64, pair Pair, changed *int64) error {
	// A single legacy polygon row cannot truthfully represent two subnests.
	// Publish its species only while it maps to one logical group.
	_, err := tx.ExecContext(ctx, `UPDATE nests n JOIN talonflame_nest_sources s ON s.source_nest_id=n.nest_id
	 SET n.pokemon_id=?,n.pokemon_form=?,n.last_changed=?,n.updated=?
	 WHERE s.group_id=? AND NOT EXISTS
	 (SELECT 1 FROM talonflame_nest_sources other WHERE other.source_nest_id=n.nest_id AND other.group_id<>?)`, pair.Pokemon, pair.Form, changed, time.Now().Unix(), id, id)
	return err
}

func (a *Analyzer) Groups() []Group {
	result := make([]Group, 0)
	for _, group := range a.groups {
		if group.Pair.Pokemon > 0 {
			copy := *group
			// Internally retain the last established label for change detection
			// and membership repair; externally a mixed polygon is unconfirmed.
			copy.Confirmed = group.Confirmed && !group.Conflicting
			result = append(result, copy)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
