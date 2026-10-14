package nat

import (
	"errors"
	"net/netip"
	"testing"
)

func TestIsPrivate(t *testing.T) {
	for s, want := range map[string]bool{
		"10.1.2.3": true, "172.16.0.1": true, "192.168.1.1": true, "127.0.0.1": true,
		"169.254.1.1": true, "100.64.0.1": true, "100.127.255.255": true, "fc00::1": true,
		"fe80::1": true, "::1": true, "::ffff:10.0.0.1": true,
		"203.0.113.1": false, "8.8.8.8": false, "100.128.0.1": false, "2001:db8::1": false,
	} {
		if got := IsPrivate(netip.MustParseAddr(s)); got != want {
			t.Errorf("IsPrivate(%s)=%v want %v", s, got, want)
		}
	}
	if IsPrivate(netip.Addr{}) {
		t.Error("invalid addr is not private")
	}
}

func TestLocalAddrToward(t *testing.T) {
	a, err := LocalAddrToward(netip.MustParseAddr("127.0.0.1"))
	if err != nil || !a.IsLoopback() {
		t.Fatalf("got %v err %v", a, err)
	}
	if _, err := LocalAddrToward(netip.Addr{}); err == nil {
		t.Fatal("expected error for invalid destination")
	}
}

type fakeSTUN struct {
	calls   []string
	answers map[string]netip.AddrPort
}

func (f *fakeSTUN) query(server string) (netip.AddrPort, error) {
	f.calls = append(f.calls, server)
	if ap, ok := f.answers[server]; ok {
		return ap, nil
	}
	return netip.AddrPort{}, errors.New("timeout")
}

var (
	localAP  = netip.MustParseAddrPort("192.168.1.50:20000")
	pubHop   = netip.MustParseAddr("203.0.113.1")
	mapped   = netip.MustParseAddrPort("198.51.100.4:41000")
	publicIP = netip.MustParseAddr("198.51.100.99")
)

func TestDecidePrivateNextHopUsesLocalWithoutSTUN(t *testing.T) {
	f := &fakeSTUN{answers: map[string]netip.AddrPort{"a:1": mapped}}
	s := NewSelector(ModeAuto, []string{"a:1"}, netip.Addr{})
	d := s.Decide(f.query, localAP, netip.MustParseAddr("10.0.0.1"))
	if len(f.calls) != 0 {
		t.Fatalf("unexpected STUN traffic %v", f.calls)
	}
	if d.Mode != ModeLocal || d.Media != localAP || d.Local != localAP.Addr() || d.STUNFailed || d.Warning != "" {
		t.Fatalf("%+v", d)
	}
}

func TestDecidePublicNextHopUsesSTUN(t *testing.T) {
	f := &fakeSTUN{answers: map[string]netip.AddrPort{"a:1": mapped}}
	s := NewSelector(ModeAuto, []string{"a:1"}, netip.Addr{})
	d := s.Decide(f.query, localAP, pubHop)
	if d.Mode != ModeSTUN || d.Media != mapped || d.STUNServer != "a:1" || d.STUNFailed {
		t.Fatalf("%+v", d)
	}
}

func TestDecideFailoverRemembersLastGood(t *testing.T) {
	f := &fakeSTUN{answers: map[string]netip.AddrPort{"b:2": mapped}}
	s := NewSelector(ModeSTUN, []string{"a:1", "b:2"}, netip.Addr{})
	d := s.Decide(f.query, localAP, pubHop)
	if d.STUNServer != "b:2" || d.Media != mapped {
		t.Fatalf("%+v", d)
	}
	if len(f.calls) != 2 || f.calls[0] != "a:1" || f.calls[1] != "b:2" {
		t.Fatalf("calls %v", f.calls)
	}
	f.calls = nil
	s.Decide(f.query, localAP, pubHop)
	if len(f.calls) != 1 || f.calls[0] != "b:2" {
		t.Fatalf("second round calls %v, want b:2 first", f.calls)
	}
}

func TestDecideAllDeadWithPublicIP(t *testing.T) {
	f := &fakeSTUN{}
	s := NewSelector(ModeSTUN, []string{"a:1", "b:2"}, publicIP)
	d := s.Decide(f.query, localAP, pubHop)
	want := netip.AddrPortFrom(publicIP, localAP.Port())
	if !d.STUNFailed || d.Warning == "" || d.Media != want {
		t.Fatalf("%+v", d)
	}
}

func TestDecideAllDeadNoPublicIP(t *testing.T) {
	f := &fakeSTUN{}
	s := NewSelector(ModeAuto, []string{"a:1"}, netip.Addr{})
	d := s.Decide(f.query, localAP, pubHop)
	if !d.STUNFailed || d.Warning == "" || d.Media != localAP {
		t.Fatalf("%+v", d)
	}
}

func TestDecidePublicIPMode(t *testing.T) {
	f := &fakeSTUN{answers: map[string]netip.AddrPort{"a:1": mapped}}
	s := NewSelector(ModePublicIP, []string{"a:1"}, publicIP)
	d := s.Decide(f.query, localAP, pubHop)
	if len(f.calls) != 0 {
		t.Fatalf("STUN used: %v", f.calls)
	}
	if d.Mode != ModePublicIP || d.Media != netip.AddrPortFrom(publicIP, localAP.Port()) || d.Warning != "" {
		t.Fatalf("%+v", d)
	}
}

func TestDecideLocalModeIgnoresPublicHop(t *testing.T) {
	f := &fakeSTUN{answers: map[string]netip.AddrPort{"a:1": mapped}}
	s := NewSelector(ModeLocal, []string{"a:1"}, netip.Addr{})
	d := s.Decide(f.query, localAP, pubHop)
	if len(f.calls) != 0 || d.Media != localAP || d.Mode != ModeLocal {
		t.Fatalf("%+v calls %v", d, f.calls)
	}
}

func TestDecideInjectedIsPrivate(t *testing.T) {
	f := &fakeSTUN{answers: map[string]netip.AddrPort{"a:1": mapped}}
	s := NewSelector(ModeAuto, []string{"a:1"}, netip.Addr{})
	s.isPrivate = func(netip.Addr) bool { return false }
	d := s.Decide(f.query, localAP, netip.MustParseAddr("10.0.0.1"))
	if d.Mode != ModeSTUN {
		t.Fatalf("%+v", d)
	}
}
