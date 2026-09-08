package helm

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestDashboardContract(t *testing.T) {
	raw, err := os.ReadFile("../../charts/shiftpv/dashboards/shiftpv-overview.json")
	if err != nil {
		t.Fatal(err)
	}
	var dashboard struct {
		UID    string
		Panels []struct {
			ID                       int
			Title, Description, Type string
			GridPos                  struct{ X, Y, W, H int }
			Datasource               struct{ UID string }
			Targets                  []struct {
				Expr           string
				Instant, Range bool
			}
		}
		Templating struct{ List []struct{ Name string } }
	}
	if err := json.Unmarshal(raw, &dashboard); err != nil {
		t.Fatal(err)
	}
	if dashboard.UID != "shiftpv-overview" {
		t.Fatal("dashboard identity or panel contract changed")
	}
	variables := map[string]bool{}
	for _, variable := range dashboard.Templating.List {
		variables[variable.Name] = true
	}
	for _, name := range []string{"DS_PROMETHEUS", "cluster", "namespace", "pool", "freshness"} {
		if !variables[name] {
			t.Fatalf("missing variable %s", name)
		}
	}
	ids := map[int]bool{}
	cells := map[[2]int]bool{}
	for _, panel := range dashboard.Panels {
		if ids[panel.ID] || panel.ID <= 0 {
			t.Fatal("invalid panel ID")
		}
		ids[panel.ID] = true
		if panel.Type == "row" {
			continue
		}
		if panel.Description == "" || panel.Datasource.UID != "${DS_PROMETHEUS}" || len(panel.Targets) == 0 {
			t.Fatalf("missing explanation, datasource or queries: %s", panel.Title)
		}
		g := panel.GridPos
		if g.X < 0 || g.Y < 0 || g.W <= 0 || g.H <= 0 || g.X+g.W > 24 {
			t.Fatal("invalid panel bounds")
		}
		for x := g.X; x < g.X+g.W; x++ {
			for y := g.Y; y < g.Y+g.H; y++ {
				cell := [2]int{x, y}
				if cells[cell] {
					t.Fatal("panels overlap")
				}
				cells[cell] = true
			}
		}
		for _, target := range panel.Targets {
			if panel.ID <= 5 && (!target.Instant || target.Range) {
				t.Fatal("overview must use current instant values, not historical lastNotNull")
			}
			if panel.ID == 2 && (!strings.Contains(target.Expr, "> bool 0") || !strings.Contains(target.Expr, "${freshness}")) {
				t.Fatal("observation status must include first-success and age guards")
			}
			if panel.ID == 14 && strings.Contains(target.Expr, "\"Age\"") && !strings.Contains(target.Expr, "> 0)") {
				t.Fatal("snapshot age must exclude never-observed timestamp zero")
			}
			for _, label := range []string{`shiftpv="true"`, `cluster=~"${cluster:regex}"`, `namespace=~"${namespace:regex}"`} {
				if !strings.Contains(target.Expr, label) {
					t.Fatalf("unscoped query: %s", target.Expr)
				}
			}
			if strings.Contains(target.Expr, "vector(0)") {
				t.Fatal("missing data must not become healthy zero")
			}
			isPool := strings.Contains(target.Expr, "shiftpv_pool_")
			if isPool != strings.Contains(target.Expr, "${pool:regex}") {
				t.Fatal("pool filter scope mismatch")
			}
		}
	}
	for id, kind := range map[int]string{1: "stat", 2: "stat", 3: "stat", 4: "stat", 5: "table", 9: "table", 10: "table", 14: "table"} {
		found := false
		for _, panel := range dashboard.Panels {
			if panel.ID == id {
				found = panel.Type == kind
			}
		}
		if !found {
			t.Fatalf("panel %d must be %s", id, kind)
		}
	}
	for _, metric := range []string{"pool_capacity_limit_bytes", "pool_reserved_bytes", "pool_unregistered_reserved_bytes",
		"pool_accounting_valid", "pool_ready", "pool_filesystem_size_bytes", "pool_filesystem_available_bytes",
		"pool_filesystem_available_inodes", "metrics_snapshot_success", "metrics_snapshot_last_success_timestamp_seconds",
		"volumes", "moves", "mobility_deferred_volumes", "csi_requests_total", "csi_request_duration_seconds_bucket"} {
		if !strings.Contains(string(raw), "shiftpv_"+metric) {
			t.Fatalf("missing metric %s", metric)
		}
	}
}

func TestDashboardConfigMap(t *testing.T) {
	raw, err := os.ReadFile("../../charts/shiftpv/dashboards/shiftpv-overview.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		args := []string{}
		if enabled {
			args = []string{"--set", "metrics.dashboard.enabled=true,metrics.dashboard.labels.team=storage"}
		}
		output, err := render(t, args...)
		if err != nil {
			t.Fatalf("%v: %s", err, output)
		}
		found := 0
		for _, doc := range strings.Split(output, "\n---") {
			var cm struct {
				Kind     string
				Metadata struct {
					Namespace string
					Labels    map[string]string
				}
				Data map[string]string
			}
			if err := yaml.Unmarshal([]byte(doc), &cm); err != nil {
				t.Fatal(err)
			}
			data, ok := cm.Data["shiftpv-overview.json"]
			if !ok {
				continue
			}
			found++
			if cm.Kind != "ConfigMap" || cm.Metadata.Namespace != "storage" ||
				cm.Metadata.Labels["grafana_dashboard"] != "1" || cm.Metadata.Labels["team"] != "storage" {
				t.Fatal("dashboard discovery metadata mismatch")
			}
			if strings.TrimSpace(data) != strings.TrimSpace(string(raw)) {
				t.Fatal("chart changed dashboard JSON")
			}
		}
		if (enabled && found != 1) || (!enabled && found != 0) {
			t.Fatal("unexpected dashboard ConfigMap count")
		}
		again, err := render(t, args...)
		if err != nil || again != output {
			t.Fatal("nondeterministic dashboard")
		}
	}
}
