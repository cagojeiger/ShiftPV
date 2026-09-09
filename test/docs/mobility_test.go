package docs_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func TestMobilityContractNamesMatchFSM(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test file location")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	source, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, "src/mobility/fsm/fsm.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "docs/spec/volume-mobility.md"))
	if err != nil {
		t.Fatal(err)
	}
	for enum, heading := range map[string]string{"Phase": "Transitions", "Action": "Actions"} {
		t.Run(enum, func(t *testing.T) {
			want := map[string]bool{}
			for _, declaration := range source.Decls {
				group, ok := declaration.(*ast.GenDecl)
				if !ok || group.Tok != token.CONST {
					continue
				}
				for _, spec := range group.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					typeName, ok := value.Type.(*ast.Ident)
					if !ok || typeName.Name != enum {
						continue
					}
					for _, expression := range value.Values {
						literal, ok := expression.(*ast.BasicLit)
						if !ok || literal.Kind != token.STRING {
							t.Fatalf("%s contract enum must have explicit string values", enum)
						}
						name, err := strconv.Unquote(literal.Value)
						if err != nil {
							t.Fatal(err)
						}
						want[name] = true
					}
				}
			}
			if len(want) == 0 {
				t.Fatalf("no %s constants found", enum)
			}
			if enum == "Phase" {
				data, err := os.ReadFile(filepath.Join(root, "charts/shiftpv/crds/shiftpv.io_shiftpvmoves.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				var crd map[string]interface{}
				if err := yaml.Unmarshal(data, &crd); err != nil {
					t.Fatal(err)
				}
				versions, found, err := unstructured.NestedSlice(crd, "spec", "versions")
				if err != nil || !found || len(versions) != 1 {
					t.Fatalf("expected one CRD version: %v", err)
				}
				version, ok := versions[0].(map[string]interface{})
				if !ok {
					t.Fatal("invalid CRD version schema")
				}
				phases, found, err := unstructured.NestedStringSlice(version, "schema", "openAPIV3Schema", "properties", "status", "properties", "phase", "enum")
				if err != nil || !found {
					t.Fatalf("missing CRD phase enum: %v", err)
				}
				seen := map[string]bool{}
				for _, phase := range phases {
					if !want[phase] || seen[phase] {
						t.Errorf("unknown or duplicate CRD phase %q", phase)
					}
					seen[phase] = true
				}
				for phase := range want {
					if !seen[phase] {
						t.Errorf("undocumented CRD phase %q", phase)
					}
				}
			}
			_, section, found := strings.Cut(string(content), "### "+heading+"\n")
			if !found {
				t.Fatalf("missing %s contract section", heading)
			}
			section, _, _ = strings.Cut(section, "\n### ")
			for _, line := range strings.Split(section, "\n") {
				cells := strings.Split(line, "|")
				if len(cells) < 3 {
					continue
				}
				cell := strings.TrimSpace(cells[1])
				if !strings.HasPrefix(cell, "`") || !strings.HasSuffix(cell, "`") {
					continue
				}
				name := strings.Trim(cell, "`")
				if !want[name] {
					t.Errorf("unknown or duplicate documented %s %q", enum, name)
				}
				delete(want, name)
			}
			for name := range want {
				t.Errorf("undocumented %s %q", enum, name)
			}
		})
	}
}
