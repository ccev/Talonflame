package talonflame

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	evidenceBatchSize  = 256
	evidenceBatchPause = 50 * time.Millisecond
)

func (a *Analyzer) deleteExpiredEvidence(ctx context.Context, cutoff int64) error {
	var deleted int64
	var batches int
	for {
		keys, err := a.expiredEvidenceKeys(ctx, cutoff)
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			break
		}
		count, err := a.deleteEvidenceBatch(ctx, keys, cutoff)
		if err != nil {
			return err
		}
		deleted += count
		batches++
		if len(keys) < evidenceBatchSize {
			break
		}
		// Pace only after COMMIT has released the batch's locks. Cancellation
		// interrupts the pause, and the next run resumes from remaining rows.
		timer := time.NewTimer(evidenceBatchPause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	if batches != 0 {
		a.logger.WithFields(logrus.Fields{"deleted": deleted, "batches": batches}).Info("expired evidence cleanup completed")
	}
	return nil
}

func (a *Analyzer) expiredEvidenceKeys(ctx context.Context, cutoff int64) ([]evidenceKey, error) {
	// This covering-index read is outside the write transaction and takes no
	// row locks. InnoDB includes the full primary key in the retention index.
	rows, err := a.db.QueryContext(ctx, `SELECT hour_start,encounter_id,spawnpoint_id
	 FROM talonflame_evidence FORCE INDEX (ix_talonflame_evidence_retention)
	 WHERE observed_at<? ORDER BY observed_at,hour_start,encounter_id,spawnpoint_id LIMIT ?`, cutoff, evidenceBatchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []evidenceKey
	for rows.Next() {
		var key evidenceKey
		if err := rows.Scan(&key.Hour, &key.Encounter, &key.Spawnpoint); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (a *Analyzer) deleteEvidenceBatch(ctx context.Context, keys []evidenceKey, cutoff int64) (int64, error) {
	if len(keys) == 0 || len(keys) > evidenceBatchSize {
		return 0, fmt.Errorf("evidence cleanup batch must contain 1..%d primary keys", evidenceBatchSize)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Hour != keys[j].Hour {
			return keys[i].Hour < keys[j].Hour
		}
		if keys[i].Encounter != keys[j].Encounter {
			return keys[i].Encounter < keys[j].Encounter
		}
		return keys[i].Spawnpoint < keys[j].Spawnpoint
	})
	args := make([]any, 0, 3*len(keys)+1)
	for _, key := range keys {
		args = append(args, key.Hour, key.Encounter, key.Spawnpoint)
	}
	args = append(args, cutoff)
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Force exact, full-PK ranges: a DELETE driven by observed_at (even with
	// LIMIT) can scan/lock far more rows than it deletes. Recheck retention in
	// case a selected observation was refreshed before acquiring its row lock.
	tuples := strings.TrimSuffix(strings.Repeat("(?,?,?),", len(keys)), ",")
	result, err := tx.ExecContext(ctx, `DELETE e FROM talonflame_evidence e FORCE INDEX (PRIMARY)
	 WHERE (e.hour_start,e.encounter_id,e.spawnpoint_id) IN (`+tuples+`) AND e.observed_at<?`, args...)
	if err != nil {
		return 0, err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}
