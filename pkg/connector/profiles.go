package connector

import (
	"context"
	"sync"
	"time"

	"github.com/markgyoni/mautrix-rift/pkg/riftclient"
)

const (
	profileTTL        = 6 * time.Hour
	profileBatchDelay = 200 * time.Millisecond
	profileBatchMax   = 100
)

// profileCache serves GetUserInfo. Lookups that arrive close together are coalesced into one
// POST /users/profiles/batch, and Shard member lists pre-fill it, so syncing a big Shard doesn't
// fire one request per ghost (which Rift rate-limits).
type profileCache struct {
	client *riftclient.Client

	lock    sync.Mutex
	entries map[string]cachedProfile
	pending map[string][]chan *riftclient.Profile
	timer   *time.Timer
}

type cachedProfile struct {
	profile *riftclient.Profile
	at      time.Time
}

func newProfileCache(client *riftclient.Client) *profileCache {
	return &profileCache{client: client, entries: map[string]cachedProfile{}, pending: map[string][]chan *riftclient.Profile{}}
}

func (pc *profileCache) put(p *riftclient.Profile) {
	pc.lock.Lock()
	pc.entries[p.ID] = cachedProfile{p, time.Now()}
	pc.lock.Unlock()
}

// get returns the profile, or nil if Rift doesn't know the user.
func (pc *profileCache) get(ctx context.Context, userID string) (*riftclient.Profile, error) {
	pc.lock.Lock()
	if e, ok := pc.entries[userID]; ok && time.Since(e.at) < profileTTL {
		pc.lock.Unlock()
		return e.profile, nil
	}
	ch := make(chan *riftclient.Profile, 1)
	pc.pending[userID] = append(pc.pending[userID], ch)
	switch {
	case len(pc.pending) >= profileBatchMax:
		if pc.timer != nil {
			pc.timer.Stop()
		}
		go pc.flush()
	case pc.timer == nil:
		pc.timer = time.AfterFunc(profileBatchDelay, pc.flush)
	}
	pc.lock.Unlock()
	select {
	case p := <-ch:
		return p, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (pc *profileCache) flush() {
	pc.lock.Lock()
	pending := pc.pending
	pc.pending = map[string][]chan *riftclient.Profile{}
	pc.timer = nil
	pc.lock.Unlock()
	if len(pending) == 0 {
		return
	}
	ids := make([]string, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	profiles, err := pc.client.GetProfiles(ctx, ids...)
	byID := make(map[string]*riftclient.Profile, len(profiles))
	if err == nil {
		now := time.Now()
		pc.lock.Lock()
		for _, p := range profiles {
			byID[p.ID] = p
			pc.entries[p.ID] = cachedProfile{p, now}
		}
		pc.lock.Unlock()
	}
	for id, chans := range pending {
		for _, ch := range chans {
			ch <- byID[id] // nil on error or unknown user
		}
	}
}
