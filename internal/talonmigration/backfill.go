package talonmigration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"hash"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/golang/geo/s2"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/geojson"
	"github.com/sirupsen/logrus"

	"github.com/UnownHash/Fletchling/geo"
)

type polygonIndex struct {
	tree   *geo.FenceRTree[int64]
	digest [32]byte
	count  int
}

type assignment struct {
	spawnpoint uint64
	nest       *int64
	status     string
	candidates []int64
}

func sourceDigestRow(h hash.Hash, id int64, polygon []byte) {
	fmt.Fprintf(h, "%d:%d:", id, len(polygon))
	h.Write(polygon)
}

func loadPolygons(ctx context.Context, conn *sql.Conn, buildIndex bool) (polygonIndex, error) {
	result := polygonIndex{tree: geo.NewFenceRTree[int64]()}
	rows, err := conn.QueryContext(ctx, "SELECT nest_id, ST_AsGeoJSON(polygon) FROM nests ORDER BY nest_id")
	if err != nil {
		return result, err
	}
	defer rows.Close()
	h := sha256.New()
	for rows.Next() {
		var id int64
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			return result, err
		}
		sourceDigestRow(h, id, data)
		result.count++
		if !buildIndex {
			continue
		}
		if id <= 0 {
			return result, fmt.Errorf("source nest %d needs a positive ID for initial logical group seeding", id)
		}
		geometry, err := geojson.UnmarshalGeometry(data)
		if err != nil {
			return result, fmt.Errorf("nest %d geometry: %w", id, err)
		}
		if err := validateGeometry(geometry.Geometry()); err != nil {
			return result, fmt.Errorf("nest %d geometry: %w", id, err)
		}
		if err := result.tree.InsertGeometry(geometry.Geometry(), id); err != nil {
			return result, fmt.Errorf("nest %d geometry: %w", id, err)
		}
	}
	copy(result.digest[:], h.Sum(nil))
	return result, rows.Err()
}

func validateGeometry(geometry orb.Geometry) error {
	var polygons orb.MultiPolygon
	switch value := geometry.(type) {
	case orb.Polygon:
		polygons = orb.MultiPolygon{value}
	case orb.MultiPolygon:
		polygons = value
	default:
		return fmt.Errorf("expected Polygon or MultiPolygon")
	}
	if len(polygons) == 0 {
		return fmt.Errorf("empty polygon")
	}
	for _, polygon := range polygons {
		if len(polygon) == 0 {
			return fmt.Errorf("empty polygon")
		}
		for _, ring := range polygon {
			if len(ring) < 4 || ring[0] != ring[len(ring)-1] {
				return fmt.Errorf("polygon rings must be closed and have at least four points")
			}
			for _, point := range ring {
				if math.IsNaN(point[0]) || math.IsInf(point[0], 0) || math.IsNaN(point[1]) || math.IsInf(point[1], 0) || math.Abs(point[0]) > 180 || math.Abs(point[1]) > 90 {
					return fmt.Errorf("invalid longitude/latitude")
				}
			}
		}
	}
	return nil
}

func matchSpawnpoint(id uint64, tree *geo.FenceRTree[int64]) assignment {
	result := assignment{spawnpoint: id, status: "invalid"}
	// The webhook/DB ID is a shortened, level-20 S2 token, not a full CellID.
	if id >= 1<<44 || id&1 == 0 {
		return result
	}
	cell := s2.CellID(id << 20)
	if !cell.IsValid() || cell.Level() != 20 {
		return result
	}
	center := cell.LatLng()
	result.candidates = tree.GetMatches(center.Lat.Degrees(), center.Lng.Degrees())
	// Deduplicate IDs defensively; source order must not affect assignment.
	sort.Slice(result.candidates, func(i, j int) bool { return result.candidates[i] < result.candidates[j] })
	unique := result.candidates[:0]
	for _, candidate := range result.candidates {
		if len(unique) == 0 || unique[len(unique)-1] != candidate {
			unique = append(unique, candidate)
		}
	}
	result.candidates = unique
	switch len(unique) {
	case 0:
		result.status = "outside"
	case 1:
		result.status = "imported"
		result.nest = &result.candidates[0]
	default:
		result.status = "ambiguous"
	}
	return result
}

func backfill(ctx context.Context, conn *sql.Conn, logger *logrus.Logger, opts Options) error {
	index, err := loadPolygons(ctx, conn, true)
	if err != nil {
		return err
	}
	if index.count == 0 {
		return fmt.Errorf("no source polygons found; import nests before backfilling")
	}
	logger.WithField("polygons", index.count).Info("indexed all existing nest polygons, including inactive and overlapping records")
	if opts.Apply {
		// Require this tool's completed schema migration, not just the legacy
		// Fletchling migration history.
		if err := schema(ctx, conn, logger, false); err != nil {
			return err
		}
		var applied int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM talonflame_schema_migrations WHERE version='001_schema.sql'").Scan(&applied); err != nil {
			return fmt.Errorf("apply schema first: %w", err)
		}
		if applied != 1 {
			return fmt.Errorf("apply schema before backfilling")
		}
		for _, table := range []string{"talonflame_backfill_assignments", "talonflame_backfill_candidates"} {
			if _, err := conn.ExecContext(ctx, "DELETE FROM "+table); err != nil {
				return fmt.Errorf("prepare staging (apply schema first): %w", err)
			}
		}
	}
	var ceiling sql.NullString
	if err := conn.QueryRowContext(ctx, "SELECT MAX(id) FROM spawnpoint").Scan(&ceiling); err != nil {
		return err
	}
	counts := map[string]uint64{}
	var havePriorState bool
	if opts.Apply {
		if err := conn.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM talonflame_spawnpoint_state LIMIT 1)").Scan(&havePriorState); err != nil {
			return err
		}
	}
	var scanned uint64
	var cursor uint64
	first := true
	lastLog := time.Now()
	for ceiling.Valid {
		rows, err := conn.QueryContext(ctx, `SELECT id FROM spawnpoint
			WHERE (? OR id > ?) AND id <= ? ORDER BY id LIMIT ?`, first, cursor, ceiling.String, opts.BatchSize)
		if err != nil {
			return err
		}
		var batch []assignment
		for rows.Next() {
			var id uint64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, matchSpawnpoint(id, index.tree))
			cursor = id
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		first = false
		if opts.Apply {
			// Most of Golbat's spawnpoints are outside parks. Don't materialize
			// millions of unnecessary outside-state/staging rows. Previously
			// tracked outside points still participate to clear stale assignments.
			tracked := map[uint64]bool{}
			if havePriorState {
				var placeholders []string
				var args []any
				for _, row := range batch {
					if row.status == "outside" {
						placeholders = append(placeholders, "?")
						args = append(args, row.spawnpoint)
					}
				}
				if len(args) > 0 {
					rows, err := conn.QueryContext(ctx, "SELECT spawnpoint_id FROM talonflame_spawnpoint_state WHERE spawnpoint_id IN ("+strings.Join(placeholders, ",")+")", args...)
					if err != nil {
						return err
					}
					for rows.Next() {
						var id uint64
						if err := rows.Scan(&id); err != nil {
							rows.Close()
							return err
						}
						tracked[id] = true
					}
					err = rows.Err()
					rows.Close()
					if err != nil {
						return err
					}
				}
			}
			var staged []assignment
			for _, row := range batch {
				if row.status != "outside" || tracked[row.spawnpoint] {
					staged = append(staged, row)
				}
			}
			if err := stageBatch(ctx, conn, staged); err != nil {
				return err
			}
		}
		for _, row := range batch {
			counts[row.status]++
		}
		scanned += uint64(len(batch))
		logger.WithFields(logrus.Fields{"scanned": scanned, "last_spawnpoint": cursor}).Debug("spawnpoint batch processed")
		if scanned%100000 < uint64(len(batch)) || time.Since(lastLog) >= 30*time.Second {
			logger.WithFields(logrus.Fields{"scanned": scanned, "counts": counts}).Info("backfill progress")
			lastLog = time.Now()
		}
	}
	if opts.Apply {
		if err := publish(ctx, conn, index.digest); err != nil {
			return err
		}
	}
	logger.WithFields(logrus.Fields{"scanned": scanned, "counts": counts, "applied": opts.Apply}).Info("polygon membership backfill completed")
	return nil
}

func stageBatch(ctx context.Context, conn *sql.Conn, batch []assignment) error {
	if len(batch) == 0 {
		return nil
	}
	var assignmentValues, candidateValues []string
	var assignmentArgs, candidateArgs []any
	for _, row := range batch {
		assignmentValues = append(assignmentValues, "(?,?,?)")
		assignmentArgs = append(assignmentArgs, row.spawnpoint, row.nest, row.status)
		for _, nest := range row.candidates {
			candidateValues = append(candidateValues, "(?,?)")
			candidateArgs = append(candidateArgs, row.spawnpoint, nest)
		}
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO talonflame_backfill_assignments (spawnpoint_id,nest_id,status) VALUES "+strings.Join(assignmentValues, ","), assignmentArgs...); err != nil {
		return err
	}
	// Bound placeholder counts even for unusually dense overlapping polygons.
	for start := 0; start < len(candidateValues); start += 1000 {
		end := start + 1000
		if end > len(candidateValues) {
			end = len(candidateValues)
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO talonflame_backfill_candidates (spawnpoint_id,source_nest_id) VALUES "+strings.Join(candidateValues[start:end], ","), candidateArgs[2*start:2*end]...); err != nil {
			return err
		}
	}
	return nil
}

func publish(ctx context.Context, conn *sql.Conn, expectedDigest [32]byte) error {
	current, err := loadPolygons(ctx, conn, false)
	if err != nil {
		return err
	}
	if current.digest != expectedDigest {
		return fmt.Errorf("source polygons changed during backfill; rerun against a stable import")
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := []string{
		// Never copy heuristic species labels or nests.updated into new state.
		`INSERT INTO talonflame_nest_groups (nest_id)
		 SELECT nest_id FROM nests ON DUPLICATE KEY UPDATE nest_id=VALUES(nest_id)`,
		// Do not recreate a 1:1 source link after learned splits/merges.
		`INSERT INTO talonflame_nest_sources (group_id,source_nest_id)
		 SELECT n.nest_id,n.nest_id FROM nests n WHERE NOT EXISTS
		 (SELECT 1 FROM talonflame_nest_sources s WHERE s.source_nest_id=n.nest_id)`,
		// Resolve source candidates through current logical groups. This keeps
		// reimports compatible with learned merges and leaves unresolved splits
		// ambiguous, while preserving explicit per-spawnpoint learned decisions.
		`UPDATE talonflame_backfill_assignments a JOIN (
		 SELECT c.spawnpoint_id,COUNT(DISTINCT s.group_id) AS group_count,MIN(s.group_id) AS group_id
		 FROM talonflame_backfill_candidates c JOIN talonflame_nest_sources s
		 ON s.source_nest_id=c.source_nest_id GROUP BY c.spawnpoint_id
		) g ON g.spawnpoint_id=a.spawnpoint_id
		 SET a.nest_id=IF(g.group_count=1,g.group_id,NULL),
		 a.status=IF(g.group_count=1,'imported','ambiguous')`,
		`DELETE c FROM talonflame_spawnpoint_candidates c
		 JOIN talonflame_backfill_assignments a ON a.spawnpoint_id=c.spawnpoint_id`,
		`INSERT INTO talonflame_spawnpoint_candidates (spawnpoint_id,source_nest_id)
		 SELECT spawnpoint_id,source_nest_id FROM talonflame_backfill_candidates`,
		`UPDATE spawnpoint p JOIN talonflame_backfill_assignments a ON a.spawnpoint_id=p.id
		 LEFT JOIN talonflame_spawnpoint_state s ON s.spawnpoint_id=p.id
		 SET p.talonflame_nest_id=a.nest_id
		 WHERE s.status IS NULL OR s.status NOT IN ('rejected','learned')`,
		`INSERT INTO talonflame_spawnpoint_state (spawnpoint_id,nest_id,status,reason)
		 SELECT spawnpoint_id,nest_id,status,NULL FROM talonflame_backfill_assignments
		 ON DUPLICATE KEY UPDATE
		 nest_id=IF(talonflame_spawnpoint_state.status IN ('rejected','learned'),talonflame_spawnpoint_state.nest_id,VALUES(nest_id)),
		 reason=IF(talonflame_spawnpoint_state.status IN ('rejected','learned'),talonflame_spawnpoint_state.reason,NULL),
		 status=IF(talonflame_spawnpoint_state.status IN ('rejected','learned'),talonflame_spawnpoint_state.status,VALUES(status))`,
	}
	for _, query := range queries {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("publish membership: %w", err)
		}
	}
	return tx.Commit()
}
