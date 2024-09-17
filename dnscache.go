package dnscache

import (
	"context"
	"net"
	"net/http/httptrace"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type DNSResolver interface {
	LookupHost(ctx context.Context, host string) (addrs []string, err error)
	LookupAddr(ctx context.Context, addr string) (names []string, err error)
}

type Resolver struct {
	// Timeout defines the maximum allowed time allowed for a lookup.
	Timeout time.Duration

	// Resolver is used to perform actual DNS lookup. If nil,
	// net.DefaultResolver is used instead.
	Resolver DNSResolver

	once  sync.Once
	mu    sync.RWMutex
	cache map[string]*cacheEntry
}

type cacheEntry struct {
	rrs  []string
	used bool
}

// LookupAddr performs a reverse lookup for the given address, returning a list
// of names mapping to that address.
func (r *Resolver) LookupAddr(ctx context.Context, addr string) (names []string, err error) {
	r.once.Do(r.init)
	return r.lookup(ctx, "r"+addr)
}

// LookupHost looks up the given host using the local resolver. It returns a
// slice of that host's addresses.
func (r *Resolver) LookupHost(ctx context.Context, host string) (addrs []string, err error) {
	r.once.Do(r.init)
	return r.lookup(ctx, "h"+host)
}

// Remove removes the entry for the given host from the cache.
func (r *Resolver) Remove(host string) {
	r.once.Do(r.init)
	r.mu.Lock()
	delete(r.cache, "h"+host)
	r.mu.Unlock()
}

// refreshRecords refreshes cached entries which have been used at least once since
// the last Refresh.
func (r *Resolver) refreshRecords() {
	r.once.Do(r.init)
	r.mu.RLock()
	update := make([]string, 0, len(r.cache))
	del := make([]string, 0, len(r.cache))
	for key, entry := range r.cache {
		if entry.used {
			update = append(update, key)
		} else {
			del = append(del, key)
		}
	}
	r.mu.RUnlock()

	if len(del) > 0 {
		r.mu.Lock()
		for _, key := range del {
			delete(r.cache, key)
		}
		r.mu.Unlock()
	}

	for _, key := range update {
		r.update(context.Background(), key, false)
	}
}

func (r *Resolver) Refresh() {
	r.refreshRecords()
}

func (r *Resolver) init() {
	r.cache = make(map[string]*cacheEntry)
}

// lookupGroup merges lookup calls together for lookups for the same host. The
// lookupGroup key is is the LookupIPAddr.host argument.
var lookupGroup singleflight.Group

func (r *Resolver) lookup(ctx context.Context, key string) (rrs []string, err error) {
	var found bool
	rrs, found = r.load(key)
	if !found {
		rrs, err = r.update(ctx, key, true)
	}
	return
}

func (r *Resolver) update(ctx context.Context, key string, used bool) (rrs []string, err error) {
	c := lookupGroup.DoChan(key, r.lookupFunc(ctx, key))
	select {
	case <-ctx.Done():
		err = ctx.Err()
		if err == context.DeadlineExceeded {
			// If DNS request timed out for some reason, force future
			// request to start the DNS lookup again rather than waiting
			// for the current lookup to complete.
			lookupGroup.Forget(key)
		}
	case res := <-c:
		if res.Shared {
			// We had concurrent lookups, check if the cache is already updated
			// by a friend.
			var found bool
			rrs, found = r.load(key)
			if found {
				return
			}
		}

		if res.Err != nil {
			var found bool
			rrs, found = r.load(key)
			if found {
				return
			}
			return nil, res.Err
		}

		rrs, _ = res.Val.([]string)

		r.mu.Lock()
		r.storeLocked(key, rrs, used)
		r.mu.Unlock()
	}
	return
}

// lookupFunc returns lookup function for key. The type of the key is stored as
// the first char and the lookup subject is the rest of the key.
func (r *Resolver) lookupFunc(ctx context.Context, key string) func() (interface{}, error) {
	if len(key) == 0 {
		panic("lookupFunc with empty key")
	}

	var resolver DNSResolver = defaultResolver
	if r.Resolver != nil {
		resolver = r.Resolver
	}

	switch key[0] {
	case 'h':
		return func() (interface{}, error) {
			ctx, cancel := r.prepareCtx(ctx)
			defer cancel()

			return resolver.LookupHost(ctx, key[1:])
		}
	case 'r':
		return func() (interface{}, error) {
			ctx, cancel := r.prepareCtx(ctx)
			defer cancel()

			return resolver.LookupAddr(ctx, key[1:])
		}
	default:
		panic("lookupFunc invalid key type: " + key)
	}
}

func (r *Resolver) prepareCtx(origContext context.Context) (ctx context.Context, cancel context.CancelFunc) {
	ctx = context.Background()
	if r.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
	} else {
		cancel = func() {}
	}

	// If a httptrace has been attached to the given context it will be copied over to the newly created context. We only need to copy pointers
	// to DNSStart and DNSDone hooks
	if trace := httptrace.ContextClientTrace(origContext); trace != nil {
		derivedTrace := &httptrace.ClientTrace{
			DNSStart: trace.DNSStart,
			DNSDone:  trace.DNSDone,
		}

		ctx = httptrace.WithClientTrace(ctx, derivedTrace)
	}

	return
}

func (r *Resolver) load(key string) (rrs []string, found bool) {
	r.mu.RLock()
	var entry *cacheEntry
	entry, found = r.cache[key]
	if !found {
		r.mu.RUnlock()
		return
	}
	rrs = entry.rrs
	used := entry.used
	r.mu.RUnlock()

	if !used {
		r.mu.Lock()
		entry.used = true
		r.cache[key] = entry
		r.mu.Unlock()
	}
	return rrs, true
}

func (r *Resolver) storeLocked(key string, rrs []string, used bool) {
	if entry, found := r.cache[key]; found {
		// Update existing entry in place
		entry.rrs = rrs
		entry.used = used
		return
	}
	r.cache[key] = &cacheEntry{
		rrs:  rrs,
		used: used,
	}
}

var defaultResolver = &defaultResolverWithTrace{}

// defaultResolverWithTrace calls `LookupIP` instead of `LookupHost` on `net.DefaultResolver` in order to cause invocation of the `DNSStart`
// and `DNSDone` hooks. By implementing `DNSResolver`, backward compatibility can be ensured.
type defaultResolverWithTrace struct{}

func (d *defaultResolverWithTrace) LookupHost(ctx context.Context, host string) (addrs []string, err error) {
	// `net.Resolver#LookupHost` does not cause invocation of `net.Resolver#lookupIPAddr`, therefore the `DNSStart` and `DNSDone` tracing hooks
	// built into the stdlib are never called. `LookupIP`, despite it's name, can also be used to lookup a hostname but does cause these hooks to be
	// triggered. The format of the reponse is different, therefore it needs this thin wrapper converting it.
	rawIPs, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}

	cookedIPs := make([]string, len(rawIPs))

	for i, v := range rawIPs {
		cookedIPs[i] = v.String()
	}

	return cookedIPs, nil
}

func (d *defaultResolverWithTrace) LookupAddr(ctx context.Context, addr string) (names []string, err error) {
	return net.DefaultResolver.LookupAddr(ctx, addr)
}
