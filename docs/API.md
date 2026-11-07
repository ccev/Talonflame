# API

## Talonflame map API

Map routes are available on `http.addr`. Optionally set `http.map_addr` to a
private `host:port` to serve only these read-only routes on another listener.

`GET /api/map/nests?bbox=west,south,east,north&limit=1000`

Returns confirmed nest regions from a shared immutable snapshot. `bbox` is required
for viewport queries; antimeridian-crossing bounds are supported. `limit` is 1–10000
(default 10000). Optional `pokemon=16,276` filters species. Instead of a viewport,
use `GET /api/map/nests?id=-375` for a logical group, including negative IDs.

Talonflame group confirmation controls visibility. Legacy Fletchling `active`,
`discarded` and cached source spawnpoint counts do not gate a confirmed group:
those fields describe the old abundance-based analyzer's eligibility, not the
learned nest's validity. Provisional, conflicting and species-less groups are
still excluded. Source deletion or withdrawal of confirmation removes the region
on the next snapshot refresh; no bulk activation of imported polygons is needed.

```json
{"revision":"...","snapshot":true,"examined":1,"data":[{"id":"8103463","pokemon_id":16,"form":0,"name":"Planten un Blomen","area_name":"Hamburg","lat":53.56,"lon":9.98,"spawnpoints":1005,"m2":298865.7,"last_changed":null,"geometry":{"type":"MultiPolygon","coordinates":[]}}]}
```

Geometry retains OSM outlines, holes and multipart sections. Different confirmed
species are separated by straight geofence cuts fitted to assigned spawnpoints;
additional populations use successive cuts. Same-species/form groups share a
region, with direct-ID aliases. These are approximate display boundaries, not
persisted changes to imported geometry or learned membership.

The builder coalesces analyzer changes every five seconds and checks external
source geometry/metadata changes once per minute. Only changed parks are rebuilt;
membership is read only for mixed-species parks, and unchanged unions are reused.
Requests perform no SQL or geometry reconstruction. Viewport queries use a spatial
index and cap results by count and 16 MiB of nest data. A capped response contains
`limitReached: true` and an empty `data` array; zoom in or narrow the species filter.

`snapshot: true` means replace the previous viewport result, including on an idle
refresh. An empty snapshot removes withdrawn/conflicting nests. `revision` and the
query-specific `ETag` cover display data, geometry and removals; `last_changed` is
only the inferred species-change boundary. Send `If-None-Match` for a bodyless 304.
The API returns 503 while the initial snapshot is building or refreshes have failed
for more than two minutes. Consumers must apply their own user permissions and
clip returned geometry before exposing it to clients.

`GET /api/map/nests/stats` returns `{ "revision": "...", "data": [...] }`, with
`pokemon_id`, `form` and `count` per displayed species. It uses the same snapshot.
`GET /api/nests` retains its existing metadata-only response.

## Legacy Fletchling API

## Get config and the version of Fletchling that is running.
`curl http://localhost:9042/api/config`

## Reload configuration
`curl http://localhost:9042/api/config/reload`
(Also supports PUT. You can also send a SIGHUP signal to the process)

## Re-run spawnpoint, area, overlap filtering and reload configuration:
`curl 'http://localhost:9042/api/config/reload?refresh=1'`

## Refresh spawnpoint counts; Re-run spawnpoint, area, overlap filtering and reload configuration:
`curl 'http://localhost:9042/api/config/reload?spawnpoints=all'` (refresh=1 is implied and not required)

## Get all nests
`curl http://localhost:9042/api/nests`

## Get single nest
`curl http://localhost:9042/api/nests/:nest_id`

## Get all nests and full stats history
`curl http://localhost:9042/api/nests/_/stats`

## Get single nest and its stats history
`curl http://localhost:9042/api/nests/_/:nest_id`

## Enable debug logging

`curl http://localhost:9042/debug/logging/on`

## Disable debug logging

`curl http://localhost:9042/debug/logging/off`

Untested:

## Purge all stats
`curl -X PUT http://localhost:9042/api/stats/purge/all`

This ditches all stats history including the current time period. This starts the stats with a clean slate, but like startup.

## Purge some duration of oldest stats
`curl -X PUT http://localhost:9042/api/stats/purge/oldest -d '{ "duration_minutes": xx }'`

Purges the specified duration of the stats starting from the oldest. This will never remove the current unfinished time period. This can be used to nuke everything but the current time period by specifying a very high number of minutes.

## Purge some duration of newest stats
`curl -X PUT http://localhost:9042/api/stats/purge/newest -d '{ "duration_minutes": xx, "include_current": false }'`

Purges the specified amount of minutes of stats starting from the newest. 'include_current' specifies whether it should start with the current time period that is not done, or if it should start at the last period.

## Ensure only a certain duration of stats exists
`curl -X PUT http://localhost:9042/api/stats/purge/keep -d '{ "duration_minutes": xx }'`

This is another way to purge oldest stats. But with this one, you specify the duration to keep, not the duration to purge.

# Healthcheck status endpoint

## Get status
`curl http://localhost:9042/status`
