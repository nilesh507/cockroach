// Copyright 2014 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package kvcoord

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/biogo/store/llrb"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv/kvbase"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/util/cache"
	"github.com/cockroachdb/cockroach/pkg/util/contextutil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil/singleflight"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
	"github.com/opentracing/opentracing-go"
)

// rangeCacheKey is the key type used to store and sort values in the
// RangeCache.
type rangeCacheKey roachpb.RKey

func (a rangeCacheKey) String() string {
	return roachpb.Key(a).String()
}

// Compare implements the llrb.Comparable interface for rangeCacheKey, so that
// it can be used as a key for util.OrderedCache.
func (a rangeCacheKey) Compare(b llrb.Comparable) int {
	return bytes.Compare(a, b.(rangeCacheKey))
}

// RangeDescriptorDB is a type which can query range descriptors from an
// underlying datastore. This interface is used by rangeDescriptorCache to
// initially retrieve information which will be cached.
type RangeDescriptorDB interface {
	// RangeLookup takes a key to look up descriptors for. Two slices of range
	// descriptors are returned. The first of these slices holds descriptors
	// whose [startKey,endKey) spans contain the given key (possibly from
	// intents), and the second holds prefetched adjacent descriptors.
	RangeLookup(
		ctx context.Context, key roachpb.RKey, useReverseScan bool,
	) ([]roachpb.RangeDescriptor, []roachpb.RangeDescriptor, error)

	// FirstRange returns the descriptor for the first Range. This is the
	// Range containing all meta1 entries.
	FirstRange() (*roachpb.RangeDescriptor, error)
}

// RangeDescriptorCache is used to retrieve range descriptors for
// arbitrary keys. Descriptors are initially queried from storage
// using a RangeDescriptorDB, but are cached for subsequent lookups.
type RangeDescriptorCache struct {
	st      *cluster.Settings
	stopper *stop.Stopper
	// RangeDescriptorDB is used to retrieve range descriptors from the
	// database, which will be cached by this structure.
	db RangeDescriptorDB
	// rangeCache caches replica metadata for key ranges. The cache is
	// filled while servicing read and write requests to the key value
	// store.
	rangeCache struct {
		syncutil.RWMutex
		cache *cache.OrderedCache
	}
	// lookupRequests stores all inflight requests retrieving range
	// descriptors from the database. It allows multiple RangeDescriptorDB
	// lookup requests for the same inferred range descriptor to be
	// multiplexed onto the same database lookup. See makeLookupRequestKey
	// for details on this inference.
	lookupRequests singleflight.Group

	// coalesced, if not nil, is sent on every time a request is coalesced onto
	// another in-flight one. Used by tests to block until a lookup request is
	// blocked on the single-flight querying the db.
	coalesced chan struct{}
}

// RangeDescriptorCache implements the kvbase interface.
var _ kvbase.RangeDescriptorCache = (*RangeDescriptorCache)(nil)

type lookupResult struct {
	desc       *roachpb.RangeDescriptor
	evictToken *EvictionToken
}

// makeLookupRequestKey constructs a key for the lookupRequest group with the
// goal of mapping all requests which are inferred to be looking for the same
// descriptor onto the same request key to establish request coalescing.
//
// If the key is part of a descriptor that we previously had cached (but the
// cache entry is stale), we use that previous descriptor to coalesce all
// requests for keys within it into a single request. Namely, there are three
// possible events that may have happened causing our cache to be stale. For
// each of these, we try to coalesce all requests that will end up on the same
// range post-event together.
// - Split:  for a split, only the right half of the split will attempt to evict
//           the stale descriptor because only the right half will be sending to
//           the wrong range. Once this stale descriptor is evicted, keys from
//           both halves of the split will miss the cache. Because both sides of
//           the split will now map to the same lookupResult, it is important to
//           use EvictAndReplace if possible to insert one of the two new descriptors.
//           This way, no requests to that descriptor will ever miss the cache and
//           risk being coalesced into the other request. If this is not possible,
//           the lookup will still work, but it will require multiple lookups, which
//           will be launched in series when requests find that their desired key
//           is outside of the returned descriptor.
// - Merges: for a merge, the left half of the merge will never notice. The right
//           half of the merge will suddenly find its descriptor to be stale, so
//           it will evict and lookup the new descriptor. We set the key to hash
//           to the start of the stale descriptor for lookup requests to the right
//           half of the merge so that all requests will be coalesced to the same
//           lookupRequest.
// - Rebal:  for a rebalance, the entire descriptor will suddenly go stale and
//           requests to it will evict the descriptor. We set the key to hash to
//           the start of the stale descriptor for lookup requests to the rebalanced
//           descriptor so that all requests will be coalesced to the same lookupRequest.
//
// Note that the above description assumes that useReverseScan is false for simplicity.
// If useReverseScan is true, we need to use the end key of the stale descriptor instead.
func makeLookupRequestKey(
	key roachpb.RKey, prevDesc *roachpb.RangeDescriptor, useReverseScan bool,
) string {
	var ret strings.Builder
	// We only want meta1, meta2, user range lookups to be coalesced with other
	// meta1, meta2, user range lookups, respectively. Otherwise, deadlocks could
	// happen due to singleflight. If the range lookup is in a meta range, we
	// prefix the request key with the corresponding meta prefix to disambiguate
	// the different lookups.
	if key.AsRawKey().Compare(keys.Meta1KeyMax) < 0 {
		ret.Write(keys.Meta1Prefix)
	} else if key.AsRawKey().Compare(keys.Meta2KeyMax) < 0 {
		ret.Write(keys.Meta2Prefix)
	}
	if prevDesc != nil {
		if useReverseScan {
			key = prevDesc.EndKey
		} else {
			key = prevDesc.StartKey
		}
	}
	ret.Write(key)
	ret.WriteString(":")
	ret.WriteString(strconv.FormatBool(useReverseScan))
	// Add the generation of the previous descriptor to the lookup request key to
	// decrease the number of lookups in the rare double split case. Suppose we
	// have a range [a, e) that gets split into [a, c) and [c, e). The requests
	// on [c, e) will fail and will have to retry the lookup. If [a, c) gets
	// split again into [a, b) and [b, c), we don't want to the requests on [a,
	// b) to be coalesced with the retried requests on [c, e). To distinguish the
	// two cases, we can use the generation of the previous descriptor.
	if prevDesc != nil && prevDesc.GetGenerationComparable() {
		ret.WriteString(":")
		ret.WriteString(strconv.FormatInt(prevDesc.GetGeneration(), 10))
	}
	return ret.String()
}

// NewRangeDescriptorCache returns a new RangeDescriptorCache which
// uses the given RangeDescriptorDB as the underlying source of range
// descriptors.
func NewRangeDescriptorCache(
	st *cluster.Settings, db RangeDescriptorDB, size func() int64, stopper *stop.Stopper,
) *RangeDescriptorCache {
	rdc := &RangeDescriptorCache{st: st, db: db, stopper: stopper}
	rdc.rangeCache.cache = cache.NewOrderedCache(cache.Config{
		Policy: cache.CacheLRU,
		ShouldEvict: func(n int, _, _ interface{}) bool {
			return int64(n) > size()
		},
	})
	return rdc
}

func (rdc *RangeDescriptorCache) String() string {
	rdc.rangeCache.RLock()
	defer rdc.rangeCache.RUnlock()
	return rdc.stringLocked()
}

func (rdc *RangeDescriptorCache) stringLocked() string {
	var buf strings.Builder
	rdc.rangeCache.cache.Do(func(k, v interface{}) bool {
		fmt.Fprintf(&buf, "key=%s desc=%+v\n", roachpb.Key(k.(rangeCacheKey)), v)
		return false
	})
	return buf.String()
}

// EvictionToken holds eviction state between calls to LookupRangeDescriptor.
type EvictionToken struct {
	prevDesc *roachpb.RangeDescriptor

	doOnce    sync.Once                                               // assures that do and doReplace are run up to once.
	doLocker  sync.Locker                                             // protects do and doReplace.
	do        func(context.Context) error                             // called on eviction.
	doReplace func(context.Context, ...roachpb.RangeDescriptor) error // called after eviction on EvictAndReplace.
}

func (rdc *RangeDescriptorCache) makeEvictionToken(
	prevDesc *roachpb.RangeDescriptor, evict func(ctx context.Context) error,
) *EvictionToken {
	return &EvictionToken{
		prevDesc:  prevDesc,
		do:        evict,
		doReplace: rdc.insertRangeDescriptorsLocked,
		doLocker:  &rdc.rangeCache,
	}
}

// Evict instructs the EvictionToken to evict the RangeDescriptor it was created
// with from the rangeDescriptorCache.
func (et *EvictionToken) Evict(ctx context.Context) error {
	return et.EvictAndReplace(ctx)
}

// EvictAndReplace instructs the EvictionToken to evict the RangeDescriptor it was
// created with from the rangeDescriptorCache. It also allows the user to provide
// new RangeDescriptors to insert into the cache, all atomically. When called without
// arguments, EvictAndReplace will behave the same as Evict.
func (et *EvictionToken) EvictAndReplace(
	ctx context.Context, newDescs ...roachpb.RangeDescriptor,
) error {
	var err error
	et.doOnce.Do(func() {
		et.doLocker.Lock()
		defer et.doLocker.Unlock()
		err = et.do(ctx)
		if err == nil {
			if len(newDescs) > 0 {
				err = et.doReplace(ctx, newDescs...)
				log.Eventf(ctx, "evicting cached range descriptor with %d replacements", len(newDescs))
			} else {
				log.Event(ctx, "evicting cached range descriptor")
			}
		}
	})
	return err
}

// LookupRangeDescriptorWithEvictionToken attempts to locate a descriptor for the range
// containing the given Key. This is done by first trying the cache, and then
// querying the two-level lookup table of range descriptors which cockroach
// maintains. The function should be provided with an EvictionToken if one was
// acquired from this function on a previous lookup. If not, an empty
// EvictionToken can be provided.
//
// This method first looks up the specified key in the first level of
// range metadata, which returns the location of the key within the
// second level of range metadata. This second level location is then
// queried to retrieve a descriptor for the range where the key's
// value resides. Range descriptors retrieved during each search are
// cached for subsequent lookups.
//
// This method returns the RangeDescriptor for the range containing
// the key's data and a token to manage evicting the RangeDescriptor
// if it is found to be stale, or an error if any occurred.
func (rdc *RangeDescriptorCache) LookupRangeDescriptorWithEvictionToken(
	ctx context.Context, key roachpb.RKey, evictToken *EvictionToken, useReverseScan bool,
) (*roachpb.RangeDescriptor, *EvictionToken, error) {
	return rdc.lookupRangeDescriptorInternal(ctx, key, evictToken, useReverseScan)
}

// LookupRangeDescriptor presents a simpler interface for looking up a
// RangeDescriptor for a key without the eviction tokens or scan direction
// control of LookupRangeDescriptorWithEvictionToken. This method is exported
// to lower level clients through the kvbase.RangeDescriptorCache interface.
func (rdc *RangeDescriptorCache) LookupRangeDescriptor(
	ctx context.Context, key roachpb.RKey,
) (*roachpb.RangeDescriptor, error) {
	rd, _, err := rdc.lookupRangeDescriptorInternal(ctx, key, nil, false)
	return rd, err
}

// lookupRangeDescriptorInternal is called from LookupRangeDescriptor or from tests.
//
// If a WaitGroup is supplied, it is signaled when the request is
// added to the inflight request map (with or without merging) or the
// function finishes. Used for testing.
func (rdc *RangeDescriptorCache) lookupRangeDescriptorInternal(
	ctx context.Context, key roachpb.RKey, evictToken *EvictionToken, useReverseScan bool,
) (*roachpb.RangeDescriptor, *EvictionToken, error) {
	// Retry while we're hitting lookupCoalescingErrors.
	for {
		desc, newToken, err := rdc.tryLookupRangeDescriptor(ctx, key, evictToken, useReverseScan)
		if errors.HasType(err, (lookupCoalescingError{})) {
			log.VEventf(ctx, 2, "bad lookup coalescing; retrying: %s", err)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		return desc, newToken, nil
	}
}

// lookupCoalescingError is returned by tryLookupRangeDescriptor() when the
// descriptor database lookup failed because this request was grouped with
// another request for another key, and the grouping proved bad since that other
// request returned a descriptor that doesn't cover our request. The lookup
// should be retried.
type lookupCoalescingError struct {
	// key is the key whose range was being looked-up.
	key       roachpb.RKey
	wrongDesc *roachpb.RangeDescriptor
}

func (e lookupCoalescingError) Error() string {
	return fmt.Sprintf("key %q not contained in range lookup's "+
		"resulting descriptor %v", e.key, e.wrongDesc)
}

func newLookupCoalescingError(key roachpb.RKey, wrongDesc *roachpb.RangeDescriptor) error {
	return lookupCoalescingError{
		key:       key,
		wrongDesc: wrongDesc,
	}
}

// tryLookupRangeDescriptor can return a lookupCoalescingError.
func (rdc *RangeDescriptorCache) tryLookupRangeDescriptor(
	ctx context.Context, key roachpb.RKey, evictToken *EvictionToken, useReverseScan bool,
) (*roachpb.RangeDescriptor, *EvictionToken, error) {
	rdc.rangeCache.RLock()
	if desc, _, err := rdc.getCachedRangeDescriptorLocked(key, useReverseScan); err != nil {
		rdc.rangeCache.RUnlock()
		return nil, nil, err
	} else if desc != nil {
		rdc.rangeCache.RUnlock()
		returnToken := rdc.makeEvictionToken(desc, func(ctx context.Context) error {
			return rdc.evictCachedRangeDescriptorLocked(ctx, key, desc, useReverseScan)
		})
		return desc, returnToken, nil
	}

	if log.V(2) {
		log.Infof(ctx, "lookup range descriptor: key=%s (reverse: %t)", key, useReverseScan)
	}

	var prevDesc *roachpb.RangeDescriptor
	if evictToken != nil {
		prevDesc = evictToken.prevDesc
	}
	requestKey := makeLookupRequestKey(key, prevDesc, useReverseScan)
	resC, leader := rdc.lookupRequests.DoChan(requestKey, func() (interface{}, error) {
		var lookupRes lookupResult
		if err := rdc.stopper.RunTaskWithErr(ctx, "rangecache: range lookup", func(ctx context.Context) error {
			ctx, reqSpan := tracing.ForkCtxSpan(ctx, "range lookup")
			defer tracing.FinishSpan(reqSpan)
			// Clear the context's cancelation. This request services potentially many
			// callers waiting for its result, and using the flight's leader's
			// cancelation doesn't make sense.
			ctx = logtags.WithTags(context.Background(), logtags.FromContext(ctx))
			ctx = opentracing.ContextWithSpan(ctx, reqSpan)

			// Since we don't inherit any other cancelation, let's put in a generous
			// timeout as some protection against unavailable meta ranges.
			var rs, preRs []roachpb.RangeDescriptor
			if err := contextutil.RunWithTimeout(ctx, "range lookup", 10*time.Second,
				func(ctx context.Context) error {
					var err error
					rs, preRs, err = rdc.performRangeLookup(ctx, key, useReverseScan)
					return err
				}); err != nil {
				return err
			}

			switch len(rs) {
			case 0:
				return fmt.Errorf("no range descriptors returned for %s", key)
			case 1:
				desc := &rs[0]
				lookupRes = lookupResult{
					desc: desc,
					evictToken: rdc.makeEvictionToken(desc, func(ctx context.Context) error {
						return rdc.evictCachedRangeDescriptorLocked(ctx, key, desc, useReverseScan)
					}),
				}
			case 2:
				desc := &rs[0]
				nextDesc := rs[1]
				lookupRes = lookupResult{
					desc: desc,
					evictToken: rdc.makeEvictionToken(desc, func(ctx context.Context) error {
						return rdc.insertRangeDescriptorsLocked(ctx, nextDesc)
					}),
				}
			default:
				panic(fmt.Sprintf("more than 2 matching range descriptors returned for %s: %v", key, rs))
			}

			// We want to be assured that all goroutines which experienced a cache miss
			// have joined our in-flight request, and all others will experience a
			// cache hit. This requires atomicity across cache population and
			// notification, hence this exclusive lock.
			rdc.rangeCache.Lock()
			defer rdc.rangeCache.Unlock()

			// These need to be separate because we need to preserve the pointer to rs[0]
			// so that the compare-and-evict logic works correctly in EvictCachedRangeDescriptor.
			// An append could cause a copy, which would change the address of rs[0]. We insert
			// the prefetched descriptors first to avoid any unintended overwriting. We then
			// only insert the first desired descriptor, since any other descriptor in rs would
			// overwrite rs[0]. Instead, these are handled with the evictToken.
			if err := rdc.insertRangeDescriptorsLocked(ctx, preRs...); err != nil {
				log.Warningf(ctx, "range cache inserting prefetched descriptors failed: %v", err)
			}
			return rdc.insertRangeDescriptorsLocked(ctx, rs[:1]...)
		}); err != nil {
			return nil, err
		}
		return lookupRes, nil
	})

	// We must use DoChan above so that we can always unlock this mutex. This must
	// be done *after* the request has been added to the lookupRequests group, or
	// we risk it racing with an inflight request.
	rdc.rangeCache.RUnlock()

	if !leader {
		log.VEvent(ctx, 2, "coalesced range lookup request onto in-flight one")
		if rdc.coalesced != nil {
			rdc.coalesced <- struct{}{}
		}
	}

	// Wait for the inflight request.
	var res singleflight.Result
	select {
	case res = <-resC:
	case <-ctx.Done():
		return nil, nil, errors.Wrap(ctx.Err(), "aborted during range descriptor lookup")
	}

	var s string
	if res.Err != nil {
		s = res.Err.Error()
	} else {
		s = res.Val.(lookupResult).desc.String()
	}
	if res.Shared {
		log.Eventf(ctx, "looked up range descriptor with shared request: %s", s)
	} else {
		log.Eventf(ctx, "looked up range descriptor: %s", s)
	}
	if res.Err != nil {
		return nil, nil, res.Err
	}

	// We might get a descriptor that doesn't contain the key we're looking for
	// because of bad grouping of requests. For example, say we had a stale
	// [a-z) in the cache who's info is passed into this function as evictToken.
	// In the meantime the range has been split to [a-m),[m-z). A request for "a"
	// will be coalesced with a request for "m" in the singleflight, above, but
	// one of them will get a wrong results. We return an error that will trigger
	// a retry at a higher level inside the cache. Note that the retry might find
	// the descriptor it's looking for in the cache if it was pre-fetched by the
	// original lookup.
	lookupRes := res.Val.(lookupResult)
	if desc := lookupRes.desc; desc != nil {
		containsFn := (*roachpb.RangeDescriptor).ContainsKey
		if useReverseScan {
			containsFn = (*roachpb.RangeDescriptor).ContainsKeyInverted
		}
		if !containsFn(desc, key) {
			return nil, nil, newLookupCoalescingError(key, desc)
		}
	}
	return lookupRes.desc, lookupRes.evictToken, nil
}

// performRangeLookup handles delegating the range lookup to the cache's
// RangeDescriptorDB.
func (rdc *RangeDescriptorCache) performRangeLookup(
	ctx context.Context, key roachpb.RKey, useReverseScan bool,
) ([]roachpb.RangeDescriptor, []roachpb.RangeDescriptor, error) {
	// Tag inner operations.
	ctx = logtags.AddTag(ctx, "range-lookup", key)

	// In this case, the requested key is stored in the cluster's first
	// range. Return the first range, which is always gossiped and not
	// queried from the datastore.
	if keys.RangeMetaKey(key).Equal(roachpb.RKeyMin) {
		desc, err := rdc.db.FirstRange()
		if err != nil {
			return nil, nil, err
		}
		return []roachpb.RangeDescriptor{*desc}, nil, nil
	}

	return rdc.db.RangeLookup(ctx, key, useReverseScan)
}

// Clear clears all RangeDescriptors from the RangeDescriptorCache.
func (rdc *RangeDescriptorCache) Clear() {
	rdc.rangeCache.Lock()
	defer rdc.rangeCache.Unlock()
	rdc.rangeCache.cache.Clear()
}

// EvictCachedRangeDescriptor will evict any cached user-space and meta range
// descriptors for the given key. It is intended that this method be called from
// a consumer of rangeDescriptorCache through the EvictionToken abstraction if
// the returned range descriptor is discovered to be stale.
//
// seenDesc should always be passed in if available, and is used as the basis of
// a pointer-based compare-and-evict strategy. This means that if the cache does
// not contain the provided descriptor, no descriptor will be evicted. If
// seenDesc is nil, eviction is unconditional.
//
// `inverted` determines the behavior at the range boundary, similar to how it
// does in GetCachedRangeDescriptor.
func (rdc *RangeDescriptorCache) EvictCachedRangeDescriptor(
	ctx context.Context, descKey roachpb.RKey, seenDesc *roachpb.RangeDescriptor, inverted bool,
) error {
	rdc.rangeCache.Lock()
	defer rdc.rangeCache.Unlock()
	return rdc.evictCachedRangeDescriptorLocked(ctx, descKey, seenDesc, inverted)
}

// evictCachedRangeDescriptorLocked is like evictCachedRangeDescriptor, but it
// assumes that the caller holds a write lock on rdc.rangeCache.
func (rdc *RangeDescriptorCache) evictCachedRangeDescriptorLocked(
	ctx context.Context, descKey roachpb.RKey, seenDesc *roachpb.RangeDescriptor, inverted bool,
) error {
	cachedDesc, entry, err := rdc.getCachedRangeDescriptorLocked(descKey, inverted)
	if err != nil || cachedDesc == nil {
		return err
	}

	// Note that we're doing a "compare-and-erase": If seenDesc is not nil, we
	// want to clean the cache only if it equals the cached range descriptor. We
	// try to use Generation and GenerationComparable to determine if the range
	// descriptors are equal, but if we cannot, we fallback to
	// pointer-comparison. If the range descriptors are not equal, then likely
	// some other caller already evicted previously, and we can save work by not
	// doing it again (which would prompt another expensive lookup).
	if seenDesc != nil {
		if seenDesc.GetGenerationComparable() && cachedDesc.GetGenerationComparable() {
			if seenDesc.GetGeneration() != cachedDesc.GetGeneration() {
				return nil
			}
		} else if !seenDesc.GetGenerationComparable() && !cachedDesc.GetGenerationComparable() {
			if seenDesc != cachedDesc {
				return nil
			}
		} else {
			// One descriptor's generation is comparable, while the other is
			// incomparable, so the descriptors are guaranteed to be different.
			return nil
		}
	}

	if log.V(2) {
		log.Infof(ctx, "evict cached descriptor: key=%s desc=%s", descKey, cachedDesc)
	}
	rdc.rangeCache.cache.DelEntry(entry)
	return nil
}

// GetCachedRangeDescriptor retrieves the descriptor of the range which contains
// the given key. It returns nil if the descriptor is not found in the cache.
//
// `inverted` determines the behavior at the range boundary: If set to true
// and `key` is the EndKey and StartKey of two adjacent ranges, the first range
// is returned instead of the second (which technically contains the given key).
func (rdc *RangeDescriptorCache) GetCachedRangeDescriptor(
	key roachpb.RKey, inverted bool,
) (*roachpb.RangeDescriptor, error) {
	rdc.rangeCache.RLock()
	defer rdc.rangeCache.RUnlock()
	desc, _, err := rdc.getCachedRangeDescriptorLocked(key, inverted)
	return desc, err
}

// getCachedRangeDescriptorLocked is like GetCachedRangeDescriptor, but it
// assumes that the caller holds a read lock on rdc.rangeCache.
//
// In addition to GetCachedRangeDescriptor, it also returns an internal cache
// Entry that can be used for descriptor eviction.
func (rdc *RangeDescriptorCache) getCachedRangeDescriptorLocked(
	key roachpb.RKey, inverted bool,
) (*roachpb.RangeDescriptor, *cache.Entry, error) {
	// The cache is indexed using the end-key of the range, but the
	// end-key is non-inverted by default.
	var metaKey roachpb.RKey
	if !inverted {
		metaKey = keys.RangeMetaKey(key.Next())
	} else {
		metaKey = keys.RangeMetaKey(key)
	}

	entry, ok := rdc.rangeCache.cache.CeilEntry(rangeCacheKey(metaKey))
	if !ok {
		return nil, nil, nil
	}
	desc := entry.Value.(*roachpb.RangeDescriptor)

	containsFn := (*roachpb.RangeDescriptor).ContainsKey
	if inverted {
		containsFn = (*roachpb.RangeDescriptor).ContainsKeyInverted
	}

	// Return nil if the key does not belong to the range.
	if !containsFn(desc, key) {
		return nil, nil, nil
	}
	return desc, entry, nil
}

// InsertRangeDescriptors inserts the provided descriptors in the cache.
// This is a no-op for the descriptors that are already present in the cache.
func (rdc *RangeDescriptorCache) InsertRangeDescriptors(
	ctx context.Context, rs ...roachpb.RangeDescriptor,
) error {
	rdc.rangeCache.Lock()
	defer rdc.rangeCache.Unlock()
	return rdc.insertRangeDescriptorsLocked(ctx, rs...)
}

// insertRangeDescriptorsLocked is like InsertRangeDescriptors, but it assumes
// that the caller holds a write lock on rdc.rangeCache.
func (rdc *RangeDescriptorCache) insertRangeDescriptorsLocked(
	ctx context.Context, rs ...roachpb.RangeDescriptor,
) error {
	for i := range rs {
		// Note: we append the end key of each range to meta records
		// so that calls to rdc.rangeCache.cache.Ceil() for a key will return
		// the correct range.

		// Before adding a new descriptor, make sure we clear out any
		// pre-existing, overlapping descriptor which might have been
		// re-inserted due to concurrent range lookups.
		continueWithInsert, err := rdc.clearOverlappingCachedRangeDescriptors(ctx, &rs[i])
		if err != nil || !continueWithInsert {
			return err
		}
		rangeKey := keys.RangeMetaKey(rs[i].EndKey)
		if log.V(2) {
			log.Infof(ctx, "adding descriptor: key=%s desc=%s", rangeKey, &rs[i])
		}
		rdc.rangeCache.cache.Add(rangeCacheKey(rangeKey), &rs[i])
	}
	return nil
}

// clearOverlappingCachedRangeDescriptors looks up and clears any cache entries
// which overlap the specified descriptor, unless the descriptor is already in
// the cache.
//
// This method is expected to be used in preparation of inserting a descriptor
// in the cache; the bool return value specifies if the insertion should go on:
// if any overlapping descriptor is known to be newer than the one passed in,
// false is returned, and true otherwise. Note that even if false is returned,
// stale range descriptors can still be deleted from the cache.
func (rdc *RangeDescriptorCache) clearOverlappingCachedRangeDescriptors(
	ctx context.Context, desc *roachpb.RangeDescriptor,
) (bool, error) {
	startMeta := keys.RangeMetaKey(desc.StartKey)
	endMeta := keys.RangeMetaKey(desc.EndKey)
	var entriesToEvict []*cache.Entry
	continueWithInsert := true

	// Try to clear the descriptor that covers the end key of desc, if any. For
	// example, if we are inserting a [/Min, "m") descriptor, we should check if
	// we should evict an existing [/Min, /Max) descriptor.
	entry, ok := rdc.rangeCache.cache.CeilEntry(rangeCacheKey(endMeta))
	if ok {
		cached := entry.Value.(*roachpb.RangeDescriptor)
		// It might be possible that the range descriptor immediately following
		// desc.EndKey does not contain desc.EndKey, so we explicitly check that it
		// overlaps. For example, if we are inserting ["a", "c"), we don't want to
		// check ["c", "d"). We do, however, want to check ["b", "c"), which is why
		// the end key is inclusive.
		if cached.StartKey.Less(desc.EndKey) && !cached.EndKey.Less(desc.EndKey) {
			if desc.GetGenerationComparable() && cached.GetGenerationComparable() {
				if desc.GetGeneration() <= cached.GetGeneration() {
					// Generations are comparable and a newer descriptor already exists in
					// cache.
					continueWithInsert = false
				}
			} else if desc.Equal(*cached) {
				// Generations are incomparable so we don't continue with insertion
				// only if the descriptor already exists.
				continueWithInsert = false
			}
			if continueWithInsert {
				entriesToEvict = append(entriesToEvict, entry)
			}
		}
	}

	// Try to clear any descriptors whose end key is contained by the descriptor
	// we are inserting. We iterate from the range meta key after
	// RangeMetaKey(desc.StartKey) to RangeMetaKey(desc.EndKey) to avoid clearing
	// the descriptor that ends when desc starts. For example, if we are
	// inserting ["b", "c"), we should not evict ["a", "b").
	//
	// Descriptors could be cleared from the cache in the event of a merge or a
	// lot of concurrency. For example, if ranges ["a", "b") and ["b", "c") are
	// merged, we should clear both of these if we are inserting ["a", "c").
	//
	// We can usually tell which descriptor is older based on the Generation, but
	// in the legacy case in which we can't, we clear the descriptors in the
	// cache unconditionally.
	rdc.rangeCache.cache.DoRangeEntry(func(e *cache.Entry) bool {
		descriptor := e.Value.(*roachpb.RangeDescriptor)
		if desc.GetGenerationComparable() && descriptor.GetGenerationComparable() {
			// If generations are comparable, then check generations to see if we
			// evict.
			if desc.GetGeneration() <= descriptor.GetGeneration() {
				continueWithInsert = false
			} else {
				entriesToEvict = append(entriesToEvict, e)
			}
		} else {
			// If generations are not comparable, evict.
			entriesToEvict = append(entriesToEvict, e)
		}
		return false
	}, rangeCacheKey(startMeta.Next()), rangeCacheKey(endMeta))

	for _, e := range entriesToEvict {
		if log.V(2) {
			log.Infof(ctx, "clearing overlapping descriptor: key=%s desc=%s",
				e.Key, e.Value.(*roachpb.RangeDescriptor))
		}
		rdc.rangeCache.cache.DelEntry(e)
	}
	return continueWithInsert, nil
}
