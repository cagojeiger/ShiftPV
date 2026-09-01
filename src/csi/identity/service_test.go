package identity

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestServiceAdvertisesOnlyImplementedCapabilities(t *testing.T) {
	service := &Service{Version: "test-version"}
	info, err := service.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != DriverName || info.VendorVersion != "test-version" {
		t.Fatalf("unexpected plugin info: %#v", info)
	}

	response, err := service.GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[csi.PluginCapability_Service_Type]bool{
		csi.PluginCapability_Service_CONTROLLER_SERVICE:               true,
		csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS: true,
	}
	if len(response.Capabilities) != len(want) {
		t.Fatalf("unexpected capability count: %d", len(response.Capabilities))
	}
	for _, capability := range response.Capabilities {
		serviceCapability := capability.GetService()
		if serviceCapability == nil || !want[serviceCapability.Type] {
			t.Fatalf("unexpected capability: %#v", capability)
		}
		delete(want, serviceCapability.Type)
	}
	if len(want) != 0 {
		t.Fatalf("missing capabilities: %#v", want)
	}
	if _, err := service.Probe(context.Background(), &csi.ProbeRequest{}); err != nil {
		t.Fatal(err)
	}
}
