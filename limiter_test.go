package smtp

import (
	"fmt"
	"net"
	"sync"
	"testing"
)

func testLimiterCounts(t *testing.T, lim *MemoryLimiter, ips, users int) {
	t.Helper()

	lim.locker.Lock()
	defer lim.locker.Unlock()

	ipCount, userCount := 0, 0
	for _, n := range lim.ips {
		ipCount += n
	}
	for _, n := range lim.users {
		userCount += n
	}
	if ipCount != ips || userCount != users {
		t.Fatalf("Invalid limiter counts: got %d IPs and %d users, want %d IPs and %d users",
			ipCount, userCount, ips, users)
	}
}

func TestMemoryLimiterPerIP(t *testing.T) {
	lim := &MemoryLimiter{MaxPerIP: 2}
	ip := net.ParseIP("192.0.2.1")

	l1, err := lim.LimitConn(ip)
	if err != nil {
		t.Fatal("LimitConn:", err)
	}
	l2, err := lim.LimitConn(ip)
	if err != nil {
		t.Fatal("LimitConn:", err)
	}
	if _, err := lim.LimitConn(ip); err == nil {
		t.Fatal("LimitConn did not fail with the per-IP limit reached")
	}
	// A different source IP has its own quota.
	l3, err := lim.LimitConn(net.ParseIP("192.0.2.2"))
	if err != nil {
		t.Fatal("LimitConn for another IP:", err)
	}

	l1.Free()
	l4, err := lim.LimitConn(ip)
	if err != nil {
		t.Fatal("LimitConn after Free:", err)
	}

	l4.Free()
	l4.Free() // Free is idempotent
	l2.Free()
	l3.Free()
	testLimiterCounts(t, lim, 0, 0)
}

func TestMemoryLimiterIPv4Mapped(t *testing.T) {
	lim := &MemoryLimiter{MaxPerIP: 1}

	lease, err := lim.LimitConn(net.ParseIP("192.0.2.1"))
	if err != nil {
		t.Fatal("LimitConn:", err)
	}

	// All representations of the same IPv4 address share one quota.
	aliases := []net.IP{
		net.ParseIP("::ffff:192.0.2.1"),
		net.ParseIP("::ffff:c000:201"),
		net.IPv4(192, 0, 2, 1),
		net.ParseIP("192.0.2.1").To4(),
	}
	for _, ip := range aliases {
		if _, err := lim.LimitConn(ip); err == nil {
			t.Errorf("LimitConn(%v) did not fail, IPv4-mapped forms must share the source quota", ip)
		}
	}

	lease.Free()
	if _, err := lim.LimitConn(net.ParseIP("::ffff:192.0.2.1")); err != nil {
		t.Fatal("LimitConn after Free:", err)
	}
	testLimiterCounts(t, lim, 1, 0)
}

func TestMemoryLimiterNilIP(t *testing.T) {
	lim := &MemoryLimiter{MaxPerIP: 1}

	if _, err := lim.LimitConn(nil); err != nil {
		t.Fatal("LimitConn:", err)
	}
	// Connections without a source IP share a single bucket.
	if _, err := lim.LimitConn(nil); err == nil {
		t.Fatal("LimitConn did not fail for the shared nil-IP bucket")
	}
}

func TestMemoryLimiterSetUser(t *testing.T) {
	lim := &MemoryLimiter{MaxPerIP: 1, MaxPerUser: 1}

	l1, err := lim.LimitConn(net.ParseIP("192.0.2.1"))
	if err != nil {
		t.Fatal("LimitConn:", err)
	}
	l2, err := lim.LimitConn(net.ParseIP("192.0.2.2"))
	if err != nil {
		t.Fatal("LimitConn:", err)
	}

	if err := l1.SetUser("user1"); err != nil {
		t.Fatal("SetUser:", err)
	}
	// The connection moved to the user group, freeing its per-IP slot.
	testLimiterCounts(t, lim, 1, 1)

	// The user group is full: the transfer fails and l2 stays in the
	// source group instead of overselling the user quota.
	if err := l2.SetUser("user1"); err == nil {
		t.Fatal("SetUser did not fail with the per-user limit reached")
	}
	testLimiterCounts(t, lim, 1, 1)

	// A different user still has quota.
	if err := l2.SetUser("user2"); err != nil {
		t.Fatal("SetUser for another user:", err)
	}
	testLimiterCounts(t, lim, 0, 2)

	l1.Free()
	testLimiterCounts(t, lim, 0, 1)
	l2.Free()
	testLimiterCounts(t, lim, 0, 0)
}

func TestMemoryLimiterUnsetUser(t *testing.T) {
	lim := &MemoryLimiter{MaxPerIP: 1, MaxPerUser: 1}

	lease, err := lim.LimitConn(net.ParseIP("192.0.2.1"))
	if err != nil {
		t.Fatal("LimitConn:", err)
	}
	if err := lease.SetUser("user1"); err != nil {
		t.Fatal("SetUser:", err)
	}
	testLimiterCounts(t, lim, 0, 1)

	// Simulates the STARTTLS state reset.
	lease.UnsetUser()
	testLimiterCounts(t, lim, 1, 0)

	// The connection can authenticate again.
	if err := lease.SetUser("user1"); err != nil {
		t.Fatal("SetUser after UnsetUser:", err)
	}
	testLimiterCounts(t, lim, 0, 1)

	lease.UnsetUser()
	lease.UnsetUser() // UnsetUser is idempotent
	lease.Free()
	testLimiterCounts(t, lim, 0, 0)
}

func TestMemoryLimiterSetUserConcurrent(t *testing.T) {
	const n = 16

	lim := &MemoryLimiter{MaxPerIP: n, MaxPerUser: 1}

	leases := make([]ConnLimitLease, n)
	for i := range leases {
		lease, err := lim.LimitConn(net.ParseIP(fmt.Sprintf("192.0.2.%d", i+1)))
		if err != nil {
			t.Fatal("LimitConn:", err)
		}
		leases[i] = lease
	}

	// All connections complete AUTH at the same time: exactly one of
	// them may enter the full user group.
	var wg sync.WaitGroup
	errs := make([]error, n)
	barrier := make(chan struct{})
	for i := range leases {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-barrier
			errs[i] = leases[i].SetUser("user1")
		}(i)
	}
	close(barrier)
	wg.Wait()

	succeeded := 0
	for i, err := range errs {
		if err == nil {
			succeeded++
			leases[i].Free()
		}
	}
	if succeeded != 1 {
		t.Fatalf("SetUser succeeded %d times, want exactly 1", succeeded)
	}
	testLimiterCounts(t, lim, n-1, 0)

	// The leases that lost the race are still accounted for in the
	// source group and release it correctly.
	for _, lease := range leases {
		lease.Free()
	}
	testLimiterCounts(t, lim, 0, 0)
}

func TestMemoryLimiterConcurrentChurn(t *testing.T) {
	lim := &MemoryLimiter{MaxPerIP: 4, MaxPerUser: 2}
	ip := net.ParseIP("2001:db8::1")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				lease, err := lim.LimitConn(ip)
				if err != nil {
					continue
				}
				lease.SetUser(fmt.Sprintf("user%d", i%3))
				if j%2 == 0 {
					lease.UnsetUser()
				}
				lease.Free()
			}
		}(i)
	}
	wg.Wait()

	// Whatever the interleaving was, all counts must be back to zero.
	testLimiterCounts(t, lim, 0, 0)
}

func TestConnRemoteIP(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	go func() {
		c, err := l.Accept()
		if err == nil {
			c.Close()
		}
	}()

	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	ip := connRemoteIP(c)
	if ip == nil || !ip.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatal("connRemoteIP returned an unexpected IP:", ip)
	}
}
