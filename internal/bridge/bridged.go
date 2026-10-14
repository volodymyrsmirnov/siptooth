package bridge

import (
	"github.com/volodymyrsmirnov/siptooth/internal/hfp"
	"github.com/volodymyrsmirnov/siptooth/internal/sip"
)

// Placeholders completed in Task 16.
func (c *Coordinator) startBridged(call *activeCall)                        {}
func (c *Coordinator) onBridgedTimer(call *activeCall, name string)         {}
func (c *Coordinator) onTelBridged(call *activeCall, ev hfp.Event)          {}
func (c *Coordinator) sipEndedBridged(call *activeCall, cause sip.EndCause) {}
func (c *Coordinator) onPumpEnd(m msg)                                      {}
func (c *Coordinator) onPanic(what string, stack []byte)                    {}
func (c *Coordinator) onGoroutinePanic(m msg)                               {}
