package weblogin

import (
	"errors"
	"io"
	"net/http"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

func TestOutboundDialGuardEnforcedOnResolvedAddress(t *testing.T) {
	var hits atomic.Int32
	srv, pool := newTestTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, "ok")
	}))
	url := "https://127.0.0.1:" + serverPort(srv) + "/"

	denied := newOutboundClient(Outbound{RootCAs: pool}, []string{"127.0.0.1"})
	if _, err := denied.Get(url); !errors.Is(err, errDialDenied) {
		t.Fatalf("loopback without exception: err=%v, want errDialDenied", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("server reached %d times despite denial", hits.Load())
	}

	allowed := newOutboundClient(Outbound{RootCAs: pool, AllowPrivateNetwork: true}, []string{"127.0.0.1"})
	resp, err := allowed.Get(url)
	if err != nil {
		t.Fatalf("loopback with private exception: %v", err)
	}
	resp.Body.Close()
	if hits.Load() != 1 {
		t.Fatalf("hits=%d, want 1", hits.Load())
	}
}

func TestOutboundRefusesPlainHTTPAndUnlistedHosts(t *testing.T) {
	var hits atomic.Int32
	srv, pool := newTestTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	c := newOutboundClient(Outbound{RootCAs: pool, TestDialContext: testDial(srv)}, []string{idpHost})
	for _, u := range []string{
		"http://" + idpHost + ":" + serverPort(srv) + "/",
		hostURL(otherHost, srv, "/"),
		"https://user:pw@" + idpHost + ":" + serverPort(srv) + "/",
	} {
		if resp, err := c.Get(u); err == nil {
			resp.Body.Close()
			t.Errorf("request to %q allowed", u)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("server reached %d times", hits.Load())
	}
	resp, err := c.Get(hostURL(idpHost, srv, "/"))
	if err != nil {
		t.Fatalf("allowed host: %v", err)
	}
	resp.Body.Close()
}

func TestOutboundRefusesRedirectsAndCapsBodies(t *testing.T) {
	var targetHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusFound)
	})
	mux.HandleFunc("/target", func(w http.ResponseWriter, r *http.Request) { targetHits.Add(1) })
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 2048))
	})
	mux.HandleFunc("/small", func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 1024))
	})
	srv, pool := newTestTLSServer(t, mux)
	c := newOutboundClient(Outbound{RootCAs: pool, TestDialContext: testDial(srv), MaxBodyBytes: 1024}, []string{idpHost})

	resp, err := c.Get(hostURL(idpHost, srv, "/redirect"))
	if err != nil {
		t.Fatalf("redirect: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || targetHits.Load() != 0 {
		t.Fatalf("redirect followed: status=%d targetHits=%d", resp.StatusCode, targetHits.Load())
	}

	resp, err = c.Get(hostURL(idpHost, srv, "/big"))
	if err != nil {
		t.Fatalf("big: %v", err)
	}
	if _, err := io.ReadAll(resp.Body); !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("oversized body read err=%v, want errBodyTooLarge", err)
	}
	resp.Body.Close()

	resp, err = c.Get(hostURL(idpHost, srv, "/small"))
	if err != nil {
		t.Fatalf("small: %v", err)
	}
	if b, err := io.ReadAll(resp.Body); err != nil || len(b) != 1024 {
		t.Fatalf("body at cap: len=%d err=%v", len(b), err)
	}
	resp.Body.Close()
}

func TestOutboundTimeoutBoundsHangingProvider(t *testing.T) {
	release := make(chan struct{})
	srv, pool := newTestTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer close(release)
	c := newOutboundClient(Outbound{RootCAs: pool, TestDialContext: testDial(srv), Timeout: 200 * time.Millisecond}, []string{idpHost})
	start := time.Now()
	resp, err := c.Get(hostURL(idpHost, srv, "/"))
	if err == nil {
		resp.Body.Close()
		t.Fatal("hanging provider returned a response")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("request took %v, want bounded by the 200ms timeout", d)
	}
}

func TestCheckDialAddr(t *testing.T) {
	public := []string{"8.8.8.8", "20.190.151.68", "2001:4860:4860::8888", "2606:4700::1111", "::ffff:8.8.8.8"}
	private := []string{
		"127.0.0.1", "::1", "10.0.0.5", "172.16.3.4", "192.168.1.10",
		"100.64.0.1", "fd12:3456::1", "::ffff:10.1.2.3", "::ffff:127.0.0.1",
		"64:ff9b::a00:1",   // NAT64 of 10.0.0.1
		"2002:c0a8:101::1", // 6to4 of 192.168.1.1
	}
	always := []string{
		"169.254.169.254", "169.254.1.1", "fe80::1", "::ffff:169.254.169.254",
		"64:ff9b::a9fe:a9fe", // NAT64 of 169.254.169.254
		"2002:a9fe:a9fe::1",  // 6to4 of 169.254.169.254
		"fd00:ec2::254",      // AWS IMDS IPv6 (inside fc00::/7)
		"100.100.100.200",    // Alibaba metadata (inside 100.64/10)
		"168.63.129.16",      // Azure wireserver (public range)
		"0.0.0.0", "0.1.2.3", "::", "224.0.0.1", "ff02::1", "255.255.255.255", "240.0.0.1",
	}
	for _, s := range public {
		ip := netip.MustParseAddr(s)
		if err := checkDialAddr(ip, false); err != nil {
			t.Errorf("public %s denied: %v", s, err)
		}
	}
	for _, s := range private {
		ip := netip.MustParseAddr(s)
		if err := checkDialAddr(ip, false); err == nil {
			t.Errorf("private %s allowed without exception", s)
		}
		if err := checkDialAddr(ip, true); err != nil {
			t.Errorf("private %s denied with exception: %v", s, err)
		}
	}
	for _, s := range always {
		ip := netip.MustParseAddr(s)
		for _, allow := range []bool{false, true} {
			if err := checkDialAddr(ip, allow); err == nil {
				t.Errorf("%s allowed (allowPrivate=%v)", s, allow)
			}
		}
	}
}

// IPv6 forms that carry an IPv4 address the guard does not decode are denied
// outright, whatever IPv4 address they embed.
func TestCheckDialAddrDeniesUndecodedEmbeddedIPv4(t *testing.T) {
	for _, s := range []string{
		"::169.254.169.254", "::127.0.0.1", "::8.8.8.8", "::1:2", // IPv4-compatible ::/96
		"::ffff:0:a9fe:a9fe", "::ffff:0:808:808", // IPv4-translated ::ffff:0:0/96
		"2001:0:4136:e378:8000:63bf:5601:5601", "2001::1", // Teredo 2001::/32
		"64:ff9b:1::a9fe:a9fe", "64:ff9b:1:ffff::808:808", // NAT64 local-use 64:ff9b:1::/48
	} {
		ip := netip.MustParseAddr(s)
		for _, allow := range []bool{false, true} {
			if err := checkDialAddr(ip, allow); err == nil {
				t.Errorf("%s allowed (allowPrivate=%v)", s, allow)
			}
		}
	}
	// Neighbours of the denied prefixes keep their normal classification.
	for _, s := range []string{"2001:1::1", "2001:4860:4860::8888", "64:ff9b:2::1", "::1:0:0:1"} {
		if err := checkDialAddr(netip.MustParseAddr(s), false); err != nil {
			t.Errorf("public %s denied: %v", s, err)
		}
	}
	if err := checkDialAddr(netip.MustParseAddr("::1"), true); err != nil {
		t.Errorf("::1 with private exception denied: %v", err)
	}
}
