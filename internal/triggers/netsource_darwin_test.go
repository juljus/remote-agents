package triggers

import "testing"

func TestSystemNetworkReadsDefaultRoute(t *testing.T) {
	snap, err := SystemNetwork()
	if err != nil {
		t.Fatalf("SystemNetwork: %v", err)
	}
	if len(snap.Addrs) == 0 {
		t.Fatal("no interface addresses found")
	}
	t.Logf("fingerprint:\n%s", snap.Fingerprint())
}
