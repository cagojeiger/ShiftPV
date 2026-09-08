# Dashboard query fixtures

| Cluster selector | Synthetic state |
|---|---|
| `preview-healthy` | Two fresh Pools, 64Mi unregistered reservation |
| `preview-degraded` | One failed scrape/probe and invalid accounting, one blocked Volume/Move |
| `preview-stale` | Last observation 10 minutes ago |
| `preview-never` | One node has never completed a successful observation |
| `preview-missing` | No metrics |

```bash
bash test/helm/dashboard/run.sh
```

The runner creates a temporary Prometheus bound to a random loopback port, validates all dashboard queries,
checks current-value semantics, and removes its container. It requires Docker, Go, curl and jq.
These recording rules are synthetic test data and stay outside the Helm package.

For visual checks, provision the product JSON in a temporary Grafana and use this Prometheus as its
datasource. Inspect Pool joins, Unknown cells, state text/color and Pool-only filtering in each scenario.
