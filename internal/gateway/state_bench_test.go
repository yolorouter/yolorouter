package gateway

import (
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/gateway/circuit"
)

// Benchmarks for the state seam: the three state hot paths, each
// measured twice — once against the concrete component and once
// through st(), exactly as the kernel reaches it. The pairs differ
// only in how the call is reached; the component and the workload
// are the same, so the delta is the seam's price (view construction
// plus interface dispatch) and nothing else.

func benchKeys(n int) []ProviderKey {
	keys := make([]ProviderKey, 0, n)
	for i := 1; i <= n; i++ {
		keys = append(keys, ProviderKey{ID: uint(i), ProviderID: 7, SortOrder: i})
	}
	return keys
}

func benchCandidates(n int) []ModelCandidate {
	cands := make([]ModelCandidate, 0, n)
	for i := 1; i <= n; i++ {
		cands = append(cands, ModelCandidate{ID: uint(i), ProviderID: uint(i), SortOrder: i})
	}
	return cands
}

func benchBreaker() *circuit.Breaker {
	return circuit.New(circuit.Config{
		FailureThreshold: 5,
		SuccessThreshold: 2,
		OpenTimeout:      time.Minute,
	})
}

func BenchmarkBreakerAllowDirect(b *testing.B) {
	br := benchBreaker()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		br.Allow(7, 0)
	}
}

func BenchmarkBreakerAllowViaSeam(b *testing.B) {
	svc := &Service{breaker: benchBreaker()}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		svc.st().Breaker().Allow(7, 0)
	}
}

func BenchmarkWalkOrderDirect(b *testing.B) {
	pool := newKeyPool(time.Now)
	keys := benchKeys(8)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pool.walkOrder(7, keys)
	}
}

func BenchmarkWalkOrderViaSeam(b *testing.B) {
	svc := &Service{keyPool: newKeyPool(time.Now)}
	keys := benchKeys(8)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		svc.st().Keys().walkOrder(7, keys)
	}
}

func BenchmarkBindingRouteDirect(b *testing.B) {
	reg := NewBindingRegistry(time.Now)
	cands := benchCandidates(8)
	alive := func(uint) bool { return false }
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reg.Route(1, 2, cands, alive)
	}
}

func BenchmarkBindingRouteViaSeam(b *testing.B) {
	svc := &Service{bindings: NewBindingRegistry(time.Now)}
	cands := benchCandidates(8)
	alive := func(uint) bool { return false }
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		svc.st().Bindings().Route(1, 2, cands, alive)
	}
}
