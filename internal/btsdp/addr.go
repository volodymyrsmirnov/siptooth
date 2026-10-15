package btsdp

import (
	"fmt"
	"net"
)

func parseMAC(mac string) ([6]byte, error) {
	hw, err := net.ParseMAC(mac)
	if err != nil || len(hw) != 6 {
		return [6]byte{}, fmt.Errorf("btsdp: bad Bluetooth address %q", mac)
	}
	var a [6]byte
	copy(a[:], hw)
	return a, nil
}

// L2Addr returns the address in display order ("AA:BB:..." gives {AA,BB,...}), which is what
// unix.SockaddrL2.Addr takes (x/sys reverses it into the kernel's order).
func L2Addr(mac string) ([6]byte, error) { return parseMAC(mac) }

// RFCOMMAddr returns the address reversed ({FF,EE,...}), which is what unix.SockaddrRFCOMM.Addr
// takes (x/sys copies it as is).
func RFCOMMAddr(mac string) ([6]byte, error) {
	a, err := parseMAC(mac)
	if err != nil {
		return a, err
	}
	for i := 0; i < 3; i++ {
		a[i], a[5-i] = a[5-i], a[i]
	}
	return a, nil
}
