package rtp

import "testing"

func TestSRTPKeyLength(t *testing.T) {
	if _, err := newSendContext(make([]byte, 29)); err == nil {
		t.Fatal("29-byte send key accepted")
	}
	if _, err := newRecvContext(make([]byte, 31)); err == nil {
		t.Fatal("31-byte receive key accepted")
	}
	if _, err := newSendContext(make([]byte, 30)); err != nil {
		t.Fatal(err)
	}
}
