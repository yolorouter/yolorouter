package router

import (
	"testing"

	"github.com/yolorouter/yolorouter/internal/config"
	"github.com/yolorouter/yolorouter/internal/gateway"
	"github.com/yolorouter/yolorouter/pkg/crypto"
)

// TestAdmissionRosterIsPinned pins which admissions are assembled, in order.
//
// Admissions acquire in registration order and release in reverse, so this
// sequence IS the compensation order. Stack discipline guarantees the reversal
// but says nothing about which order is being reversed — that is decided by the
// order of statements in registerCapabilities, and statement order is not
// something anything can otherwise check.
//
// What it catches TODAY is narrower than that, and worth stating plainly: with
// one admission registered there is no order to get wrong, so today this fails
// only when an admission is added, removed, or renamed. That is the case that
// matters right now — adding the second one makes this test fail, and updating
// the list is where its placement relative to the first has to be decided
// rather than inherited from wherever the new line happened to be typed. From
// that point on it is a real ordering check.
func TestAdmissionRosterIsPinned(t *testing.T) {
	svc := gateway.NewService(nil, crypto.SecretBox{}, false, nil, config.GatewayConfig{})
	registerCapabilities(svc, nil, "")

	want := []string{"ratelimit"}
	got := svc.RegisteredAdmissions()

	if len(got) != len(want) {
		t.Fatalf("registered admissions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("admission %d = %q, want %q (acquisition order, so compensation runs in reverse)",
				i, got[i], want[i])
		}
	}
}
