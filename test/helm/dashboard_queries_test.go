package helm

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDashboardQueries(t *testing.T) {
	endpoint := os.Getenv("SHIFTPV_DASHBOARD_PROMETHEUS_URL")
	if endpoint == "" {
		t.Skip("set SHIFTPV_DASHBOARD_PROMETHEUS_URL to the synthetic preview Prometheus")
	}
	raw, err := os.ReadFile("../../charts/shiftpv/dashboards/shiftpv-overview.json")
	if err != nil {
		t.Fatal(err)
	}
	var dashboard struct {
		Panels []struct {
			ID      int
			Targets []struct{ Expr string }
		}
	}
	if err := json.Unmarshal(raw, &dashboard); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	type result struct {
		Metric map[string]string
		Value  []json.RawMessage
	}
	query := func(t *testing.T, expr, scenario string) []result {
		t.Helper()
		expr = strings.NewReplacer("${cluster:regex}", "preview-"+scenario,
			"${namespace:regex}", "shiftpv-system", "${pool:regex}", ".*",
			"${freshness}", "180", "$__rate_interval", "1m").Replace(expr)
		response, err := client.Get(endpoint + "/api/v1/query?query=" + url.QueryEscape(expr))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var body struct {
			Status, Error string
			Data          struct{ Result []result }
		}
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != 200 || body.Status != "success" {
			t.Fatalf("%s: %s", expr, body.Error)
		}
		return body.Data.Result
	}
	for _, scenario := range []string{"healthy", "degraded", "stale", "never", "missing"} {
		t.Run(scenario, func(t *testing.T) {
			for _, panel := range dashboard.Panels {
				for targetIndex, target := range panel.Targets {
					rows := query(t, target.Expr, scenario)
					if scenario == "missing" && len(rows) != 0 {
						t.Fatalf("panel %d fabricated missing data", panel.ID)
					}
					if panel.ID >= 1 && panel.ID <= 4 && scenario != "missing" {
						expected := map[string][]string{
							"healthy":  {"1", "1", "0", "0"},
							"degraded": {"0", "0", "1", "1"},
							"stale":    {"1", "0", "", ""},
							"never":    {"1", "0", "0", "0"},
						}[scenario][panel.ID-1]
						if expected == "" {
							if len(rows) != 0 {
								t.Fatalf("panel %d: stale current value was not masked", panel.ID)
							}
						} else if len(rows) != 1 || string(rows[0].Value[1]) != "\""+expected+"\"" {
							t.Fatalf("panel %d: expected %s, got %+v", panel.ID, expected, rows)
						}
					}
					if panel.ID == 5 {
						// The available-space column must keep only fresh, reachable node observations.
						if targetIndex == 2 {
							want := map[string]int{"healthy": 2, "degraded": 1, "stale": 0, "never": 1, "missing": 0}[scenario]
							if len(rows) != want {
								t.Fatalf("space rows: got %d want %d", len(rows), want)
							}
						}
						if scenario == "stale" && targetIndex < 2 {
							if len(rows) != 2 {
								t.Fatal("stale Pools disappeared instead of Unknown state")
							}
							for _, row := range rows {
								if string(row.Value[1]) != "\"-1\"" {
									t.Fatal("stale Pool status looked healthy")
								}
							}
						}
					}
					if panel.ID == 14 && targetIndex == 1 && scenario == "never" && len(rows) != 3 {
						t.Fatal("never-observed source must have no age")
					}
				}
			}
		})
	}
	t.Run("mixed-metadata-freshness", func(t *testing.T) {
		for _, panel := range dashboard.Panels {
			if panel.ID == 3 || panel.ID == 4 {
				if rows := query(t, panel.Targets[0].Expr, "healthy|preview-stale"); len(rows) != 0 {
					t.Fatalf("panel %d: partial metadata produced a misleading aggregate", panel.ID)
				}
			}
		}
	})
	t.Run("pool-filter-and-cluster-identity", func(t *testing.T) {
		for _, panel := range dashboard.Panels {
			if panel.ID < 5 || panel.ID > 8 {
				continue
			}
			for _, target := range panel.Targets {
				expr := strings.ReplaceAll(target.Expr, "${pool:regex}", "storage-a")
				rows := query(t, expr, "healthy|preview-degraded")
				if len(rows) != 2 {
					t.Fatalf("panel %d: expected one selected Pool per cluster, got %d rows", panel.ID, len(rows))
				}
				clusters := make(map[string]bool)
				for _, row := range rows {
					if row.Metric["pool"] != "storage-a" || row.Metric["node"] != "worker-a" {
						t.Fatalf("panel %d: Pool filter leaked: %+v", panel.ID, row.Metric)
					}
					clusters[row.Metric["cluster"]] = true
				}
				if !clusters["preview-healthy"] || !clusters["preview-degraded"] {
					t.Fatalf("panel %d: same-named Pools lost cluster identity", panel.ID)
				}
			}
		}
	})
}
