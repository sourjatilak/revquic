// SPDX-License-Identifier: GPL-3.0-or-later

package userstore

import (
	"fmt"
	"testing"

	"github.com/sourjatilak/revquic/internal/adminapi"
)

// BenchmarkAuthenticateForRegion measures the per-connect auth hot path (token hash + lookup +
// region check) against a populated store.
func BenchmarkAuthenticateForRegion(b *testing.B) {
	s := New("pepper")
	const n = 1000
	for i := 0; i < n; i++ {
		_, _ = s.Create(adminapi.UserCreate{
			Username:       fmt.Sprintf("user-%d", i),
			Credential:     fmt.Sprintf("tok-%d", i),
			AllowedRegions: []string{"us-west"},
		})
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := s.AuthenticateForRegion("tok-500", "us-west"); err != nil {
			b.Fatalf("auth: %v", err)
		}
	}
}

// BenchmarkConcurrentAuth exercises the RLock path under parallelism (run with -race).
func BenchmarkConcurrentAuth(b *testing.B) {
	s := New("pepper")
	_, _ = s.Create(adminapi.UserCreate{Username: "u", Credential: "tok", AllowedRegions: []string{"*"}})
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = s.AuthenticateForRegion("tok", "any")
		}
	})
}
