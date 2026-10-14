// Package hfptest provides a scripted fake Audio Gateway (the phone side of an
// HFP service-level connection) for tests that need a real hfp.Client.
package hfptest

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// DefaultCIND is the indicator list of a typical phone: service=1, call=2,
// callsetup=3, battchg=4, signal=5, roam=6, callheld=7.
const DefaultCIND = `+CIND: ("service",(0,1)),("call",(0,1)),("callsetup",(0,3)),("battchg",(0,5)),("signal",(0,5)),("roam",(0,1)),("callheld",(0,2))`

// Indicator indexes of DefaultCIND, for +CIEV lines.
const (
	IdxCall      = 2
	IdxCallSetup = 3
)

// AG answers HF commands from a script and can inject unsolicited lines.
type AG struct {
	t        testing.TB
	conn     net.Conn
	mu       sync.Mutex
	replies  map[string][]string // command -> lines sent before "OK"
	received []string
}

// New starts a fake AG with the commands of a normal service-level connection
// scripted, and returns it with the hands-free side of the in-memory
// transport (give that to hfp.New).
func New(t testing.TB, cindList string) (*AG, net.Conn) {
	t.Helper()
	agSide, hfSide := net.Pipe()
	ag := &AG{t: t, conn: agSide, replies: map[string][]string{
		"AT+BRSF=52":      {"+BRSF: 4095"},
		"AT+CIND=?":       {cindList},
		"AT+CIND?":        {"+CIND: 1,0,0,5,4,0,0"},
		"AT+CMER=3,0,0,1": nil,
		"AT+CHLD=?":       {"+CHLD: (0,1,2,3)"},
		"AT+CLIP=1":       nil,
		"AT+VGS=15":       nil,
		"AT+VGM=15":       nil,
		"ATA":             nil,
		"AT+CHUP":         nil,
	}}
	go ag.serve()
	t.Cleanup(func() { agSide.Close(); hfSide.Close() })
	return ag, hfSide
}

func (ag *AG) serve() {
	r := bufio.NewReader(ag.conn)
	for {
		cmd, err := r.ReadString('\r')
		if err != nil {
			return
		}
		cmd = strings.TrimSpace(cmd)
		ag.mu.Lock()
		ag.received = append(ag.received, cmd)
		lines, ok := ag.replies[cmd]
		ag.mu.Unlock()
		if !ok {
			ag.Send("ERROR")
			continue
		}
		for _, l := range lines {
			ag.Send(l)
		}
		ag.Send("OK")
	}
}

// Send injects a result code line.
func (ag *AG) Send(line string) { ag.conn.Write([]byte("\r\n" + line + "\r\n")) }

// Got returns the commands received so far.
func (ag *AG) Got() []string {
	ag.mu.Lock()
	defer ag.mu.Unlock()
	return append([]string(nil), ag.received...)
}

// Count returns how many times cmd was received.
func (ag *AG) Count(cmd string) int {
	n := 0
	for _, c := range ag.Got() {
		if c == cmd {
			n++
		}
	}
	return n
}

// WaitCommand waits up to timeout for cmd to have been received.
func (ag *AG) WaitCommand(cmd string, timeout time.Duration) bool {
	for end := time.Now().Add(timeout); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
		if ag.Count(cmd) > 0 {
			return true
		}
	}
	return false
}
