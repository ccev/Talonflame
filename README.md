# Talonflame

A modern, experimental nest analyzer, built on [Fletchling](https://github.com/UnownHash/Fletchling). Unlike Fletchling's abundance-based approach, Talonflame uses recently discovered encounter ID deriving to analyze nests.

## Setup

Requires Go 1.22+, MariaDB, and Golbat with imported nest polygons and spawnpoints.

1. Clone and build

   ```sh
   git clone https://github.com/ccev/Talonflame
   cd Talonflame
   make
   ```

2. Configure

   ```sh
   cp config.example.toml config.toml
   ```

   Set your database credentials and HTTP address in `config.toml`. Use a dedicated database user; set `database.socket` for a Unix socket. Analyzer settings are under `[tuning]`; set `logging.level = "debug"` for encounter-level logs.

3. Set up database access

   Create the user as a database administrator, replacing the password:

   ```sql
   CREATE USER 'talon'@'localhost' IDENTIFIED BY 'change-me';
   ```

   Apply the table-specific grants (adjust the database/account in the script if needed):

   ```sh
   mariadb -u root -p golbat < scripts/grant-talon.sql
   ```

   Talonflame adds its own tables, `spawnpoint.talonflame_nest_id`, and `nests.last_changed`. It needs full access to `nests`, restricted access to `spawnpoint`, and access to its own tables.

4. Prepare the database

   Stop Fletchling and any nest importer first. Preview, then apply the schema and spawnpoint backfill using the configured account:

   ```sh
   ./talonflame-db-migrate
   ./talonflame-db-migrate -apply
   ```

5. Run

   ```sh
   ./talonflame
   ```

   Point Golbat's `pokemon` or `pokemon_iv` webhook at `http://127.0.0.1:9042/webhook` (match your configured address). Health, metrics, and inferred nests are available at `/healthz`, `/metrics`, and `/api/nests`.

For Docker, copy `docker-compose.yml.example` to `docker-compose.yml` and run `docker compose up -d --build` after database preparation. The example uses host networking and a local MariaDB socket.
