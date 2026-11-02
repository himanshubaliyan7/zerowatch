package cluster

import "testing"

func owners(r *Ring, n int) map[uint64]string {
	out := map[uint64]string{}
	var x uint64 = 12345
	for i := 0; i < n; i++ {
		x = x*6364136223846793005 + 1442695040888963407
		o, _ := r.Get(x)
		out[x] = o
	}
	return out
}

// TestRebalanceOnJoinLeave checks that adding a member only moves keys *to*
// the new member (roughly 1/n of them) and removing it only moves keys that
// it owned, so existing shards are never shuffled between surviving nodes.
func TestRebalanceOnJoinLeave(t *testing.T) {
	r := NewRing(128)
	r.Add("a")
	r.Add("b")
	r.Add("c")
	const keys = 20000
	before := owners(r, keys)

	r.Add("d")
	after := owners(r, keys)
	moved := 0
	for k, o := range before {
		if after[k] != o {
			moved++
			if after[k] != "d" {
				t.Fatalf("key %d moved %s->%s on join; only moves to d are allowed", k, o, after[k])
			}
		}
	}
	if share := float64(moved) / keys; share < 0.15 || share > 0.35 {
		t.Fatalf("join moved %.2f of keys, expected ~0.25", share)
	}

	r.Remove("d")
	restored := owners(r, keys)
	for k, o := range before {
		if restored[k] != o {
			t.Fatalf("key %d not restored to %s after leave (got %s)", k, o, restored[k])
		}
	}
	for k, o := range after {
		if o == "d" && restored[k] == "d" {
			t.Fatalf("key %d still owned by removed node", k)
		}
	}
}
