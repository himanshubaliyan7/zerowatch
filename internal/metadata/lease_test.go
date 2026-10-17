package metadata

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLeases(t *testing.T) {
	stores := map[string]*SQLStore{"sqlite": open(t)}
	// ZW_TEST_POSTGRES_DSN runs the same checks against a real Postgres.
	if dsn := os.Getenv("ZW_TEST_POSTGRES_DSN"); dsn != "" {
		pg, err := Open(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pg.Close() })
		stores["postgres"] = pg
	}
	for kind, s := range stores {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			name := "test-" + newID()
			must := func(want bool, holder string, ttl time.Duration) {
				t.Helper()
				got, err := s.AcquireLease(ctx, name, holder, ttl)
				if err != nil || got != want {
					t.Fatalf("AcquireLease(%s) = %v, %v; want %v", holder, got, err, want)
				}
			}
			must(true, "a", time.Minute)  // free
			must(true, "a", time.Minute)  // renew
			must(false, "b", time.Minute) // held by a
			if err := s.ReleaseLease(ctx, name, "b"); err != nil {
				t.Fatal(err)
			}
			must(false, "b", time.Minute) // b cannot release a's lease
			if err := s.ReleaseLease(ctx, name, "a"); err != nil {
				t.Fatal(err)
			}
			must(true, "b", -time.Second) // released; b takes it, already expired
			must(true, "a", time.Minute)  // expired; a takes over
			must(false, "b", time.Minute)
		})
	}
}
