package helm

import (
	"strings"
	"testing"
)

func TestMoveJournalRetention(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantFlag   string
		wantSchema bool
	}{
		{name: "default", wantFlag: "--move-journal-retention=168h"},
		{name: "one hour", args: []string{"--set", "mobility.journalRetention=1h"}, wantFlag: "--move-journal-retention=1h"},
		{name: "zero", args: []string{"--set", "mobility.journalRetention=0h"}, wantSchema: true},
		{name: "minutes", args: []string{"--set", "mobility.journalRetention=30m"}, wantSchema: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := render(t, tc.args...)
			if tc.wantSchema {
				if err == nil || !strings.Contains(output, "schema") {
					t.Fatalf("wanted schema failure: %v %s", err, output)
				}
				return
			}
			if err != nil {
				t.Fatalf("%v: %s", err, output)
			}
			if strings.Count(output, tc.wantFlag) != 1 {
				t.Fatalf("wanted exactly one %q flag", tc.wantFlag)
			}
		})
	}
}
