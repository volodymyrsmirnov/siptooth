package daemon

import (
	"log/slog"
	"sync"
	"time"

	"github.com/volodymyrsmirnov/siptooth/internal/bridge"
	"github.com/volodymyrsmirnov/siptooth/internal/sip"
)

// registrationReporter turns sip.Registration state changes into the
// coordinator's registration status and one system.error sip_registration
// per outage (spec 5.7, 10).
type registrationReporter struct {
	user      string // AOR user, for the Contact shown in status
	transport string
	status    func(bridge.RegistrationStatus)
	errors    errorSink
	log       *slog.Logger

	mu       sync.Mutex
	reported bool // an error was emitted for the current outage
}

// onState is the callback given to sip.UA.Register.
func (r *registrationReporter) onState(s sip.RegState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := bridge.RegistrationStatus{Enabled: true, Registered: s.Registered}
	if s.Contact.IsValid() {
		st.Contact = bridge.ContactURI(r.user, s.Contact, r.transport)
	}
	if s.Registered {
		if !s.Expires.IsZero() {
			exp := s.Expires
			st.Expires = &exp
		}
		if r.reported {
			r.log.Info("sip: registration restored")
		}
		r.reported = false
	} else if s.Err != nil {
		st.Error = s.Err.Error()
		if !r.reported {
			r.reported = true
			r.log.Warn("sip: registration failed", "err", s.Err)
			r.errors.EmitError("sip_registration", s.Err.Error())
		}
	}
	r.status(st)
}

// unregisterTimeout bounds REGISTER Expires: 0 at shutdown (spec 5.7).
const unregisterTimeout = 2 * time.Second
