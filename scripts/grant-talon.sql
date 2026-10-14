-- Run as a database administrator after creating talon@localhost.
GRANT ALL PRIVILEGES ON `golbat`.`nests` TO 'talon'@'localhost';
GRANT SELECT (`id`), UPDATE, ALTER ON `golbat`.`spawnpoint` TO 'talon'@'localhost';
GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER
ON `golbat`.`talonflame_schema_migrations` TO 'talon'@'localhost';
GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER
ON `golbat`.`talonflame_nest_groups` TO 'talon'@'localhost';
GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER
ON `golbat`.`talonflame_nest_sources` TO 'talon'@'localhost';
GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER
ON `golbat`.`talonflame_spawnpoint_candidates` TO 'talon'@'localhost';
GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER
ON `golbat`.`talonflame_spawnpoint_state` TO 'talon'@'localhost';
GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER
ON `golbat`.`talonflame_evidence` TO 'talon'@'localhost';
GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER
ON `golbat`.`talonflame_backfill_assignments` TO 'talon'@'localhost';
GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER
ON `golbat`.`talonflame_backfill_candidates` TO 'talon'@'localhost';
