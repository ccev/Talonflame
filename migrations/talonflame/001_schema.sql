-- These migrations target an existing Golbat/Fletchling MariaDB database.
-- last_changed is an inferred species-change boundary, in Unix seconds UTC.
-- Existing labels do not establish a historical change time: leave it NULL.
ALTER TABLE nests ADD COLUMN IF NOT EXISTS last_changed BIGINT UNSIGNED NULL;
ALTER TABLE spawnpoint ADD COLUMN IF NOT EXISTS talonflame_nest_id BIGINT NULL;
ALTER TABLE spawnpoint ADD INDEX IF NOT EXISTS ix_spawnpoint_talonflame_nest_id (talonflame_nest_id);

-- Logical groups allow a source polygon to contain multiple inferred nests,
-- or multiple source polygons to describe one inferred nest. Initial group
-- IDs equal their source nest IDs; inferred groups use negative IDs so they
-- cannot collide with a newly imported positive OSM feature ID.
CREATE TABLE IF NOT EXISTS talonflame_nest_groups (
    nest_id BIGINT NOT NULL,
    pokemon_id INT NULL,
    pokemon_form INT NULL,
    last_changed BIGINT UNSIGNED NULL,
    timezone VARCHAR(64) NULL,
    status VARCHAR(24) NOT NULL DEFAULT 'provisional',
    PRIMARY KEY (nest_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS talonflame_nest_sources (
    group_id BIGINT NOT NULL,
    source_nest_id BIGINT NOT NULL,
    PRIMARY KEY (group_id, source_nest_id),
    KEY ix_talonflame_source_nest (source_nest_id)
) ENGINE=InnoDB;

-- Keep ALL geometric candidates, including overlaps, separately from the
-- nullable active assignment on spawnpoint.
CREATE TABLE IF NOT EXISTS talonflame_spawnpoint_candidates (
    spawnpoint_id BIGINT UNSIGNED NOT NULL,
    source_nest_id BIGINT NOT NULL,
    PRIMARY KEY (spawnpoint_id, source_nest_id),
    KEY ix_talonflame_candidate_nest (source_nest_id)
) ENGINE=InnoDB;

-- status: imported, outside, ambiguous, invalid, rejected, or learned.
-- Reimports preserve rejected/learned decisions. A rejection clears the
-- active assignment without erasing its original polygon candidates.
CREATE TABLE IF NOT EXISTS talonflame_spawnpoint_state (
    spawnpoint_id BIGINT UNSIGNED NOT NULL,
    nest_id BIGINT NULL,
    status VARCHAR(24) NOT NULL,
    reason VARCHAR(255) NULL,
    PRIMARY KEY (spawnpoint_id),
    KEY ix_talonflame_state_nest (nest_id)
) ENGINE=InnoDB;

-- One vote per encounter/spawnpoint/hour. Payload corrections update the
-- existing vote. An encounter can change species across an hour boundary.
CREATE TABLE IF NOT EXISTS talonflame_evidence (
    hour_start BIGINT UNSIGNED NOT NULL,
    encounter_id BIGINT UNSIGNED NOT NULL,
    spawnpoint_id BIGINT UNSIGNED NOT NULL,
    nest_id BIGINT NULL,
    pokemon_id INT NOT NULL,
    pokemon_form INT NULL,
    roll_u24 INT UNSIGNED NOT NULL,
    observed_at BIGINT UNSIGNED NOT NULL,
    PRIMARY KEY (hour_start, encounter_id, spawnpoint_id),
    KEY ix_talonflame_evidence_nest_hour (nest_id, hour_start),
    KEY ix_talonflame_evidence_spawnpoint (spawnpoint_id, hour_start),
    KEY ix_talonflame_evidence_retention (observed_at)
) ENGINE=InnoDB;

-- Persistent staging avoids holding locks on live spawnpoints while doing
-- geometry calculations. The migration command holds an advisory lock;
-- interrupted runs can discard staging and restart without a partial map.
CREATE TABLE IF NOT EXISTS talonflame_backfill_assignments (
    spawnpoint_id BIGINT UNSIGNED NOT NULL,
    nest_id BIGINT NULL,
    status VARCHAR(24) NOT NULL,
    PRIMARY KEY (spawnpoint_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS talonflame_backfill_candidates (
    spawnpoint_id BIGINT UNSIGNED NOT NULL,
    source_nest_id BIGINT NOT NULL,
    PRIMARY KEY (spawnpoint_id, source_nest_id)
) ENGINE=InnoDB;
