package volume

import "testing"

func TestCopyIdentityValidation(t *testing.T) {
	valid := CopyIdentity{
		InstallationID: "installation", PoolName: "pool-a", PoolUID: "pool-uid",
		VolumeID: "shiftpv-11111111111111111111111111111111", VolumeUID: "volume-uid",
		CopyID: "copy-id", NodeName: "node-a", Role: RoleServing,
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*CopyIdentity){
		"installation path": func(i *CopyIdentity) { i.InstallationID = "../old" },
		"pool path":         func(i *CopyIdentity) { i.PoolName = "/pool" },
		"invalid volume":    func(i *CopyIdentity) { i.VolumeID = "volume" },
		"copy path":         func(i *CopyIdentity) { i.CopyID = "copy/other" },
		"unknown role":      func(i *CopyIdentity) { i.Role = "Unknown" },
	} {
		t.Run(name, func(t *testing.T) {
			identity := valid
			mutate(&identity)
			if err := identity.Validate(); err == nil {
				t.Fatal("invalid identity accepted")
			}
		})
	}
}
