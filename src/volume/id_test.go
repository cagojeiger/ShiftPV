package volume

import "testing"

func TestIDFromNameStableAndValid(t *testing.T) {
	a, err := IDFromName("pvc-123")
	if err != nil {
		t.Fatal(err)
	}
	b, err := IDFromName("pvc-123")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("IDs differ: %q != %q", a, b)
	}
	if err := ValidateID(a); err != nil {
		t.Fatal(err)
	}
}

func TestIDValidationRejectsUnsafeValues(t *testing.T) {
	if _, err := IDFromName(""); err == nil {
		t.Fatal("expected empty name to fail")
	}
	for _, id := range []string{
		"",
		"shiftpv-short",
		"shiftpv-0123456789ABCDEF0123456789ABCDEF",
		"shiftpv-0123456789abcdef0123456789abcdef/child",
		"../shiftpv-0123456789abcdef0123456789abcdef",
	} {
		if err := ValidateID(id); err == nil {
			t.Fatalf("expected invalid ID %q to fail", id)
		}
	}
}

func TestPathRejectsUnsafeID(t *testing.T) {
	if _, err := Path("/mnt/shiftpv", "../escape"); err == nil {
		t.Fatal("expected unsafe ID to fail")
	}
}

func TestPathRequiresAbsolutePoolRoot(t *testing.T) {
	if _, err := Path("relative", "shiftpv-0123456789abcdef0123456789abcdef"); err == nil {
		t.Fatal("expected relative pool root to fail")
	}
}

func TestIsWithin(t *testing.T) {
	if !IsWithin("/var/lib/kubelet/pods", "/var/lib/kubelet/pods/a/volumes/x") {
		t.Fatal("expected descendant path")
	}
	if IsWithin("/var/lib/kubelet/pods", "/var/lib/kubelet/pods-evil/a") {
		t.Fatal("accepted sibling path")
	}
	if IsWithin("/var/lib/kubelet/pods", "/var/lib/kubelet/pods") {
		t.Fatal("accepted root itself")
	}
}
