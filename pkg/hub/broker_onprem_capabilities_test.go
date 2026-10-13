package hub

import "testing"

func TestOnpremCapabilitiesFromRegistration(t *testing.T) {
	for _, name := range []string{"gpu", "nvidiaGpu", "nvidia_gpu"} {
		if !capabilitiesFromStrings([]string{name}).NvidiaGPU {
			t.Errorf("GPU capability %q was not registered", name)
		}
	}
	for _, name := range []string{"runtimeRecovery", "runtime_recovery"} {
		if !capabilitiesFromStrings([]string{name}).RuntimeRecovery {
			t.Errorf("retained recovery capability %q was not registered", name)
		}
	}
	caps := capabilitiesFromStrings([]string{"sync", "attach"})
	if caps.NvidiaGPU || caps.RuntimeRecovery {
		t.Fatal("unreported capabilities must remain false")
	}
}
