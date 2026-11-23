# atlantis-drift-exporter

Prometheus exporter for the [Atlantis](https://www.runatlantis.io/) drift detection status.

Atlantis keeps drift results in its API but does not expose them as Prometheus metrics. This exporter reads `GET /api/drift/status` in the background and serves the latest result on `/metrics`. Atlantis calls the VCS API on every status request, so scrapes do not reach Atlantis, and a short Atlantis or VCS outage does not remove the drift series. The exporter is read-only: it never starts a drift detection or triggers Terraform plans. Run detection separately, for example from a CronJob calling `POST /api/drift/detect`.

Written for the drift status API in Atlantis v0.50.0, which is unchanged from v0.49.0. The Atlantis drift API is experimental and may change.

## Atlantis requirements

Atlantis v0.50.0 and later:

```yaml
ATLANTIS_EXPERIMENTS: drift-detection
ATLANTIS_ENABLE_DRIFT_DETECTION: true
ATLANTIS_API_SECRET: <secret>
```

The `drift-detection` experiment is enough for this exporter and for `POST /api/drift/detect`. The `api` experiment is not needed, and leaving it out keeps `/api/plan` and `/api/apply` disabled.

Without the experiment, Atlantis refuses to start when `ATLANTIS_ENABLE_DRIFT_DETECTION` is set. If the drift routes are not enabled, they return HTTP 404 and the exporter reports `atlantis_drift_exporter_up 0`.

Atlantis v0.49.0 does not have the `experiments` setting, so omit `ATLANTIS_EXPERIMENTS` there.

## Configuration

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `ATLANTIS_URL` | yes | | Atlantis base URL |
| `ATLANTIS_API_SECRET` | yes | | Atlantis API secret |
| `ATLANTIS_REPOSITORIES` | yes | | Comma-separated repositories, e.g. `org/repo-a,org/repo-b` |
| `ATLANTIS_VCS_TYPE` | no | `Github` | `Github`, `Gitlab` or `Gitea` |
| `ATLANTIS_REF` | no | | Only report results for this git ref |
| `LISTEN_ADDRESS` | no | `:8080` | HTTP listen address |
| `REFRESH_INTERVAL` | no | `5m` | How often to read the drift status from Atlantis |
| `REFRESH_TIMEOUT` | no | `30s` | Timeout for one refresh of all repositories. Must not exceed `REFRESH_INTERVAL` |

## Metrics

| Metric | Description |
| --- | --- |
| `atlantis_drift_exporter_up{repository}` | Whether the last read of the status API succeeded |
| `atlantis_drift_exporter_last_success_timestamp_seconds{repository}` | Unix timestamp of the last successful read of the status API |
| `atlantis_drift_has_drift{...}` | Whether the last successful check found drift |
| `atlantis_drift_changes{...,action}` | Resource changes by action: `add`, `change`, `destroy`, `import`, `forget` |
| `atlantis_drift_check_failed{...}` | Whether the last check for the project failed. A pull request lock is not a failure |
| `atlantis_drift_check_locked{...}` | Whether the last check for the project was blocked by a pull request lock |
| `atlantis_drift_last_checked_timestamp_seconds{...}` | Unix timestamp of the last check, successful or not |

Project metrics have the labels `repository`, `project`, `directory`, `workspace`, `ref` and `base_branch`.

When a check fails, Atlantis reports `has_drift=false`. The exporter treats the drift state as unknown and does not publish `atlantis_drift_has_drift` or `atlantis_drift_changes` for that project.

An unapplied plan in a pull request locks its project, and Atlantis then cannot check that project for drift. The exporter reports this as `atlantis_drift_check_locked`, with no `atlantis_drift_has_drift` or `atlantis_drift_changes`. Atlantis has no error code for this case, so the exporter matches the start of the error message. If a future Atlantis version changes the message, locked projects are reported as `atlantis_drift_check_failed`.

When a refresh fails, the exporter sets `atlantis_drift_exporter_up` to `0` and keeps serving the last successful project metrics.

Atlantis stores drift results in memory, so they are lost when it restarts. After the next successful refresh, project metrics disappear until the next detection run. Alert on missing or stale data as well as on drift.

## Example alerts

```yaml
groups:
- name: atlantis-drift
  rules:
  - alert: TerraformDriftDetected
    expr: atlantis_drift_has_drift == 1
    for: 15m
    labels:
      severity: warning
    annotations:
      summary: 'Terraform drift in {{ $labels.repository }}: {{ $labels.project }} ({{ $labels.workspace }})'
  - alert: TerraformDriftCheckFailed
    expr: atlantis_drift_check_failed == 1
    for: 15m
    labels:
      severity: warning
    annotations:
      summary: 'Drift check failed in {{ $labels.repository }}: {{ $labels.project }} ({{ $labels.workspace }})'
  - alert: TerraformDriftCheckLocked
    expr: atlantis_drift_check_locked == 1
    for: 7d
    labels:
      severity: warning
    annotations:
      summary: 'Drift check blocked by a pull request lock in {{ $labels.repository }}: {{ $labels.project }} ({{ $labels.workspace }})'
  - alert: TerraformDriftCheckStale
    # Assumes detection runs at least once a day.
    expr: |
      (time() - max(atlantis_drift_last_checked_timestamp_seconds) > 26 * 3600)
      or absent(atlantis_drift_last_checked_timestamp_seconds)
    for: 30m
    labels:
      severity: warning
    annotations:
      summary: No Terraform drift check completed in the last 26 hours
  - alert: AtlantisDriftExporterDown
    expr: atlantis_drift_exporter_up == 0
    for: 15m
    labels:
      severity: warning
    annotations:
      summary: 'Cannot read Atlantis drift status for {{ $labels.repository }}'
```

## Build and run

```bash
go test ./...
docker build -t atlantis-drift-exporter .
```

```bash
docker run --rm -p 8080:8080 \
  -e ATLANTIS_URL=http://atlantis:4141 \
  -e ATLANTIS_API_SECRET \
  -e ATLANTIS_REPOSITORIES=org/repo \
  atlantis-drift-exporter
```

Endpoints: `/metrics`, `/healthz` and `/readyz`. `/readyz` returns 200 after the first refresh attempt.
