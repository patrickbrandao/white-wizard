// Package source holds the data sources of wwz-panel (spec 016, "Fontes de
// dados"):
//
//   - Valkey: read-only view of the key schema of cache/valkey/README.md
//     (entities, live limits, instances) plus the only write the panel does,
//     cfg:policies + PUBLISH cfg:changed (ADR 0025).
//   - Consumer: the ev:stream consumer of the group "panel"
//     (eventbus/valkey/asyncapi.yaml) that stores events in the panel SQLite
//     and only then XACKs them.
//   - RequestPoller: the mode without Valkey, polling GET /admin/requests.
//   - Admin: client of the wwz control plane (cmd/wwz/openapi.yaml).
//   - Prom and Metrics: Prometheus HTTP API with the PromQL of spec 016 and
//     the /admin/stats fallback.
//
// A failing source never takes the panel down: every call returns an error
// that the HTTP layer maps to 502 (see AdminError.HTTPStatus).
package source
