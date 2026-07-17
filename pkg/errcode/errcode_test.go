package errcode

import "testing"

func TestNewRouteCodesAreUniqueAndRegistered(t *testing.T) {
	newCodes := []int{RouteNotFound, MethodNotAllowed, RequestEntityTooLarge, InternalError}
	seen := map[int]bool{}
	for _, code := range newCodes {
		if code == 0 {
			t.Fatalf("code must not be zero value")
		}
		if seen[code] {
			t.Fatalf("duplicate code value: %d", code)
		}
		seen[code] = true
		if _, ok := ErrorMessages[code]; !ok {
			t.Fatalf("code %d has no entry in ErrorMessages", code)
		}
	}
}

func TestExistingSuccessCodeUnchanged(t *testing.T) {
	if Success != 0 {
		t.Fatalf("Success code must remain 0, got %d", Success)
	}
	if ErrorMessages[Success] != "success" {
		t.Fatalf("Success message must remain \"success\", got %q", ErrorMessages[Success])
	}
}
