package flagvalue

import (
	"slices"
	"testing"
)

func TestNamesCollectsUniqueTrimmedValues(t *testing.T) {
	var names Names
	for _, value := range []string{" shiftpv ", "shiftpv-retain", "shiftpv"} {
		if err := names.Set(value); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(names.Values("unused"), []string{"shiftpv", "shiftpv-retain"}) {
		t.Fatalf("names=%v", names)
	}
	if names.String() != "shiftpv,shiftpv-retain" {
		t.Fatalf("String()=%q", names.String())
	}
}

func TestNamesUsesFallbackAndRejectsEmpty(t *testing.T) {
	var names Names
	if got := names.Values("shiftpv"); !slices.Equal(got, []string{"shiftpv"}) {
		t.Fatalf("fallback=%v", got)
	}
	if err := names.Set(" "); err == nil {
		t.Fatal("empty name was accepted")
	}
}
