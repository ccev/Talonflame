-- Only assignments whose Golbat spawnpoint row was missing need reconciliation.
-- Enqueue in the same transaction as state changes; keep the current assignment
-- in talonflame_spawnpoint_state so later changes supersede earlier retries.
CREATE TABLE IF NOT EXISTS talonflame_spawnpoint_pending (
    spawnpoint_id BIGINT UNSIGNED NOT NULL,
    PRIMARY KEY (spawnpoint_id)
) ENGINE=InnoDB;
