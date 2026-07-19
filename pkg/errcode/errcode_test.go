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

func TestProviderKeyErrorCodesHaveMessagesAndSentinels(t *testing.T) {
	cases := []struct {
		code int
		err  error
	}{
		{ProviderKeyNotFound, ErrProviderKeyNotFound},
		{ProviderKeyLabelTaken, ErrProviderKeyLabelTaken},
		{ProviderKeyNotVerified, ErrProviderKeyNotVerified},
		{ProviderKeyNeedsReentry, ErrProviderKeyNeedsReentry},
	}
	for _, c := range cases {
		msg, ok := ErrorMessages[c.code]
		if !ok || msg == "" {
			t.Fatalf("code %d: missing ErrorMessages entry", c.code)
		}
		if c.err == nil || c.err.Error() != msg {
			t.Fatalf("code %d: sentinel error text %q does not match ErrorMessages %q", c.code, c.err, msg)
		}
	}
}

func TestModelErrorCodesAreUniqueWithMessagesAndSentinels(t *testing.T) {
	cases := []struct {
		code int
		err  error
	}{
		{ModelNotFound, ErrModelNotFound},
		{ModelNameTaken, ErrModelNameTaken},
		{ModelCandidateNotFound, ErrModelCandidateNotFound},
		{ModelCandidateProviderTaken, ErrModelCandidateProviderTaken},
		{ModelCandidateNotVerified, ErrModelCandidateNotVerified},
	}
	seen := map[int]bool{}
	for _, c := range cases {
		if seen[c.code] {
			t.Fatalf("duplicate code value: %d", c.code)
		}
		seen[c.code] = true
		msg, ok := ErrorMessages[c.code]
		if !ok || msg == "" {
			t.Fatalf("code %d: missing ErrorMessages entry", c.code)
		}
		if c.err == nil || c.err.Error() != msg {
			t.Fatalf("code %d: sentinel error text %q does not match ErrorMessages %q", c.code, c.err, msg)
		}
	}
}
