package metadata

import (
	"context"
	"time"
)

// AcquireLease takes or renews the named lease for holder until now+ttl. It
// succeeds when the lease is free, expired, or already held by holder, and
// reports false when another holder has a live lease. The conditional upsert
// is a single statement, so concurrent replicas cannot both win. Expiry is
// judged by the caller's clock: keep replica clocks NTP-synced, with skew well
// under the renew margin (see leader.Elector).
func (s *SQLStore) AcquireLease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	now := time.Now()
	res, err := s.exec(ctx, nil, `INSERT INTO leases (name, holder, expires_at) VALUES (?, ?, ?)
ON CONFLICT (name) DO UPDATE SET holder = excluded.holder, expires_at = excluded.expires_at
WHERE leases.holder = excluded.holder OR leases.expires_at <= ?`,
		name, holder, ms(now.Add(ttl)), ms(now))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ReleaseLease drops the named lease if holder still owns it, so a standby
// can take over at once instead of waiting out the TTL.
func (s *SQLStore) ReleaseLease(ctx context.Context, name, holder string) error {
	_, err := s.exec(ctx, nil, `DELETE FROM leases WHERE name = ? AND holder = ?`, name, holder)
	return err
}
