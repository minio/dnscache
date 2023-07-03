package dnscache

import (
	"context"
	"errors"
	"net/http/httptrace"
	"sync/atomic"
	"testing"
	"time"
)

func TestClearCache(t *testing.T) {
	r := &Resolver{}
	_, _ = r.LookupHost(context.Background(), "google.com")
	if e := r.cache["hgoogle.com"]; e != nil && !e.used {
		t.Error("cache entry used flag is false, want true")
	}
	r.Refresh()
	if e := r.cache["hgoogle.com"]; e != nil && e.used {
		t.Error("cache entry used flag is true, want false")
	}
	r.Refresh()
	if e := r.cache["hgoogle.com"]; e != nil {
		t.Error("cache entry is not cleared")
	}

	_, _ = r.LookupHost(context.Background(), "google.com")
	if e := r.cache["hgoogle.com"]; e != nil && !e.used {
		t.Error("cache entry used flag is false, want true")
	}
	r.Refresh()
	if e := r.cache["hgoogle.com"]; e != nil && e.used {
		t.Error("cache entry used flag is true, want false")
	}
	r.Refresh()
	if e := r.cache["hgoogle.com"]; e != nil {
		t.Error("cache entry is not cleared")
	}

	br := &Resolver{}
	br.Resolver = BadResolver{}

	_, _ = br.LookupHost(context.Background(), "google.com")
	br.Resolver = BadResolver{choke: true}
	br.Refresh()
	if len(br.cache["hgoogle.com"].rrs) == 0 {
		t.Error("cache entry is cleared")
	}
}

func TestRaceOnDelete(t *testing.T) {
	r := &Resolver{}
	ls := make(chan bool)
	rs := make(chan bool)

	go func() {
		for {
			select {
			case <-ls:
				return
			default:
				r.LookupHost(context.Background(), "google.com")
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	go func() {
		for {
			select {
			case <-rs:
				return
			default:
				r.Refresh()
				time.Sleep(time.Millisecond)
			}
		}
	}()

	time.Sleep(1 * time.Second)

	ls <- true
	rs <- true
}

func TestResolver_LookupHost_DNSHooksGetTriggerd(t *testing.T) {
	var (
		dnsStartInfo *httptrace.DNSStartInfo
		dnsDoneInfo  *httptrace.DNSDoneInfo
	)

	trace := &httptrace.ClientTrace{
		DNSStart: func(info httptrace.DNSStartInfo) {
			dnsStartInfo = &info
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			dnsDoneInfo = &info
		},
	}

	ctx := httptrace.WithClientTrace(context.Background(), trace)

	r := &Resolver{}

	_, err := r.LookupHost(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}

	if dnsStartInfo == nil {
		t.Error("dnsStartInfo is nil, indicating that DNSStart callback has not been invoked")
	}

	if dnsDoneInfo == nil {
		t.Error("dnsDoneInfo is nil, indicating that DNSDone callback has not been invoked")
	}
}

type fakeResolver struct {
	LookupHostCalls int32
	LookupAddrCalls int32
}

func (f *fakeResolver) LookupHost(ctx context.Context, host string) (addrs []string, err error) {
	atomic.AddInt32(&f.LookupHostCalls, 1)
	return nil, errors.New("not implemented")
}
func (f *fakeResolver) LookupAddr(ctx context.Context, addr string) (names []string, err error) {
	atomic.AddInt32(&f.LookupAddrCalls, 1)
	return nil, errors.New("not implemented")
}
