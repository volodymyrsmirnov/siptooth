package bridge

import "time"

// Call outcomes reported in call.ended and Status.LastOutcome (spec 10).
const (
	OutcomeCompleted       = "completed"
	OutcomeSIPRejected     = "sip_rejected"
	OutcomeSIPTimeout      = "sip_timeout"
	OutcomeSIPError        = "sip_error"
	OutcomeSIPMediaFailed  = "sip_media_failed"
	OutcomeCallerAbandoned = "caller_abandoned"
	OutcomeOwnerAnswered   = "owner_answered"
	OutcomeAnswerFailed    = "answer_failed"
	OutcomeFarEndEnded     = "far_end_ended"
	OutcomeSignalingLost   = "signaling_lost"
	OutcomeMediaTimeout    = "media_timeout"
	OutcomeAudioLost       = "audio_lost"
	OutcomeMediaError      = "media_error"
	OutcomeMaxDuration     = "max_duration"
	OutcomeBluetoothLost   = "bluetooth_lost"
	OutcomeShutdown        = "shutdown"
)

// Who ended a call (call.ended hangup_by, spec 10).
const (
	HangupCaller    = "caller"
	HangupFarEnd    = "far_end"
	HangupSiptooth  = "siptooth"
	hangupUndefined = ""
)

// RegistrationStatus is the SIP registration state the daemon reports with
// SetRegistration (spec 5.7). It lives here so bridge does not depend on the
// registration code.
type RegistrationStatus struct {
	Enabled    bool       `json:"enabled"`
	Registered bool       `json:"registered"`
	Contact    string     `json:"contact,omitempty"`
	Expires    *time.Time `json:"expires,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// Status is the coordinator's externally visible state (spec 11).
type Status struct {
	// Ready: the phone is connected and SCO routing is verified, so a ring
	// will be forwarded.
	Ready          bool `json:"ready"`
	PhoneConnected bool `json:"phone_connected"`
	// State is idle, dialing, answering, bridged or ending.
	State         string     `json:"state"`
	CallID        string     `json:"call_id,omitempty"`
	Caller        string     `json:"caller,omitempty"` // cleaned number, empty when withheld or no call
	CallStartedAt *time.Time `json:"call_started_at,omitempty"`
	// PhoneBusy: the phone is physically occupied (ringing, setting up a call, or carrying one),
	// whether or not siptooth forwards it. A power cycle would cut that call or ring.
	PhoneBusy bool `json:"phone_busy"`
	// CleanupPending: the late-CallActive window is armed or a background BYE / AT+CHUP is in
	// flight, so the phone or the SIP dialog may still be touched.
	CleanupPending bool `json:"cleanup_pending"`
	// LastOutcome is the outcome of the most recent call, empty before any.
	LastOutcome  string             `json:"last_outcome,omitempty"`
	Registration RegistrationStatus `json:"registration"`
}
