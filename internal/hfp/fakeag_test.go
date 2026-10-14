package hfp

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"testing"
)

// fakeAG answers HF commands from a script and can inject unsolicited lines.
type fakeAG struct {
	t        *testing.T
	conn     net.Conn
	mu       sync.Mutex
	replies  map[string][]string // command -> lines sent before "OK"
	errors   map[string]bool     // command -> reply ERROR instead of OK
	silent   map[string]bool     // command -> no reply at all
	received []string
	done     chan struct{}
}

func newFakeAG(t *testing.T, cindList string) (*fakeAG, net.Conn) {
	t.Helper()
	agSide, hfSide := net.Pipe()
	ag := &fakeAG{t: t, conn: agSide, replies: map[string][]string{}, errors: map[string]bool{}, silent: map[string]bool{}, done: make(chan struct{})}
	ag.replies["AT+BRSF=52"] = []string{"+BRSF: 4095"}
	ag.replies["AT+CIND=?"] = []string{cindList}
	ag.replies["AT+CIND?"] = []string{"+CIND: 1,0,0,5,4,0,0"}
	ag.replies["AT+CMER=3,0,0,1"] = nil
	ag.replies["AT+CHLD=?"] = []string{"+CHLD: (0,1,2,3)"}
	ag.replies["AT+CLIP=1"] = nil
	ag.replies["AT+VGS=15"] = nil
	ag.replies["AT+VGM=15"] = nil
	ag.replies["ATA"] = nil
	ag.replies["AT+CHUP"] = nil
	go ag.serve()
	return ag, hfSide
}

const defaultCIND = `+CIND: ("service",(0,1)),("call",(0,1)),("callsetup",(0,3)),("battchg",(0,5)),("signal",(0,5)),("roam",(0,1)),("callheld",(0,2))`

func (ag *fakeAG) serve() { ag.serveUntil("") }

// serveUntil answers commands like serve but stops reading after replying to last (if set).
func (ag *fakeAG) serveUntil(last string) {
	defer close(ag.done)
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
		isErr := ag.errors[cmd]
		silent := ag.silent[cmd]
		ag.mu.Unlock()
		if silent {
			continue
		}
		if !ok {
			ag.write("ERROR")
			continue
		}
		for _, l := range lines {
			ag.write(l)
		}
		if isErr {
			ag.write("ERROR")
		} else {
			ag.write("OK")
		}
		if last != "" && cmd == last {
			<-ag.t.Context().Done()
			return
		}
	}
}

func (ag *fakeAG) write(line string) {
	ag.conn.Write([]byte("\r\n" + line + "\r\n"))
}

// send injects an unsolicited result code.
func (ag *fakeAG) send(line string) { ag.write(line) }

// sendRaw writes bytes verbatim (for chunking tests).
func (ag *fakeAG) sendRaw(b string) { ag.conn.Write([]byte(b)) }

func (ag *fakeAG) got() []string {
	ag.mu.Lock()
	defer ag.mu.Unlock()
	return append([]string(nil), ag.received...)
}
