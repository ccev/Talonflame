-- Run as a database administrator AFTER applying the Talonflame schema.
-- Runtime compares only its own assignment column to skip unchanged values.
GRANT SELECT (`talonflame_nest_id`) ON `golbat`.`spawnpoint` TO 'talon'@'localhost';
