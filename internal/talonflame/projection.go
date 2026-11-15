package talonflame

import (
	"context"
	"database/sql"
	"strings"

	"github.com/sirupsen/logrus"
)

const spawnpointBatchSize = 256

// projectSpawnpoint publishes new/changed assignments in the state transaction.
// Golbat can send its webhook before inserting the spawnpoint. Only those missing
// rows need a durable retry; RowsAffected alone cannot distinguish them from
// rows whose assignment already matches (including NULL).
func projectSpawnpoint(ctx context.Context, tx *sql.Tx, point uint64, nest int64) error {
	result, err := tx.ExecContext(ctx, `UPDATE spawnpoint SET talonflame_nest_id=?
	 WHERE id=? AND NOT (talonflame_nest_id <=> ?)`, nullableNest(nest), point, nullableNest(nest))
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 0 {
		return err
	}
	var synced bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM spawnpoint
	 WHERE id=? AND talonflame_nest_id <=> ?)`, point, nullableNest(nest)).Scan(&synced); err != nil {
		return err
	}
	if synced {
		return nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO talonflame_spawnpoint_pending (spawnpoint_id) VALUES (?)
	 ON DUPLICATE KEY UPDATE spawnpoint_id=VALUES(spawnpoint_id)`, point)
	return err
}

func (a *Analyzer) reconcileSpawnpoints(ctx context.Context) error {
	var cursor uint64
	first := true
	var scanned, changed, completed int64
	for {
		// Keyset pagination visits even missing rows only once per maintenance
		// pass, so a missing low ID cannot starve later ready assignments.
		rows, err := a.db.QueryContext(ctx, `SELECT spawnpoint_id FROM talonflame_spawnpoint_pending
		 WHERE (? OR spawnpoint_id>?) ORDER BY spawnpoint_id LIMIT ?`, first, cursor, spawnpointBatchSize)
		if err != nil {
			return err
		}
		var ids []any
		for rows.Next() {
			var id uint64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		updated, removed, err := a.reconcileSpawnpointBatch(ctx, ids)
		if err != nil {
			return err
		}
		scanned += int64(len(ids))
		changed += updated
		completed += removed
		cursor = ids[len(ids)-1].(uint64)
		first = false
	}
	if scanned != 0 {
		a.logger.WithFields(logrus.Fields{"scanned": scanned, "updated": changed, "pending": scanned - completed}).Info("pending spawnpoint assignments reconciled")
	}
	return nil
}

func (a *Analyzer) reconcileSpawnpointBatch(ctx context.Context, ids []any) (int64, int64, error) {
	// Each transaction is bounded by explicit primary keys, not a LIMIT on a
	// multi-million-row join. READ COMMITTED also avoids locking gaps around
	// missing Golbat rows. A failed batch leaves all its retries intact.
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	keys := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	// Force state PK lookups to drive the join even when the pending queue grows.
	// Read the current assignment, not a potentially stale value from enqueue time.
	result, err := tx.ExecContext(ctx, `UPDATE talonflame_spawnpoint_state s FORCE INDEX (PRIMARY)
	 STRAIGHT_JOIN spawnpoint p FORCE INDEX (PRIMARY) ON p.id=s.spawnpoint_id
	 SET p.talonflame_nest_id=s.nest_id
	 WHERE s.spawnpoint_id IN (`+keys+`) AND NOT (p.talonflame_nest_id <=> s.nest_id)`, ids...)
	if err != nil {
		return 0, 0, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	// Acknowledge matching rows atomically with their update. Golbat may insert
	// a missing row between UPDATE and DELETE, so existence alone is not enough.
	// Missing or still-mismatched rows remain queued across restarts.
	result, err = tx.ExecContext(ctx, `DELETE q FROM talonflame_spawnpoint_pending q FORCE INDEX (PRIMARY)
	 STRAIGHT_JOIN talonflame_spawnpoint_state s FORCE INDEX (PRIMARY) ON s.spawnpoint_id=q.spawnpoint_id
	 STRAIGHT_JOIN spawnpoint p FORCE INDEX (PRIMARY) ON p.id=q.spawnpoint_id
	 WHERE q.spawnpoint_id IN (`+keys+`) AND p.talonflame_nest_id <=> s.nest_id`, ids...)
	if err != nil {
		return 0, 0, err
	}
	completed, err := result.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return changed, completed, nil
}
