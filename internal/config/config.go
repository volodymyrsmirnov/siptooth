// Package config loads and validates ~/.siptooth/config.yaml (spec section 9).
//
// Relative paths inside a loaded config file (control.socket) are resolved against the
// directory that holds the config file. Load decodes and resolves; Validate checks. They
// are separate because validation needs the SIP password, which lives in the environment
// or the env file (LookupPassword).
package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// KnownEvents lists hook event names (spec section 10).
var KnownEvents = []string{"call.ringing", "call.connected", "call.ended", "system.error"}

// DefaultHookTimeoutSeconds applies to a hook that sets no timeout_seconds.
const DefaultHookTimeoutSeconds = 10

type Bluetooth struct {
	Adapter      string `yaml:"adapter"`
	PhoneAddress string `yaml:"phone_address"`
	// PowerCycleMinutes power-cycles the adapter this often while no call is active; 0 turns it off.
	PowerCycleMinutes int `yaml:"power_cycle_minutes"`
}

type Phone struct {
	Number string `yaml:"number"`
}

type SIP struct {
	Target                 string            `yaml:"target"`
	Transport              string            `yaml:"transport"` // udp | tcp | tls
	Proxy                  string            `yaml:"proxy"`
	LocalPort              int               `yaml:"local_port"`
	From                   string            `yaml:"from"`
	DisplayName            string            `yaml:"display_name"`
	AnonymousUser          string            `yaml:"anonymous_user"`
	Username               string            `yaml:"username"`
	Register               bool              `yaml:"register"`
	Registrar              string            `yaml:"registrar"`
	RegisterExpiresSeconds int               `yaml:"register_expires_seconds"`
	Headers                map[string]string `yaml:"headers"`
	SRTP                   bool              `yaml:"srtp"`
	DialTimeoutSeconds     int               `yaml:"dial_timeout_seconds"`
}

type Media struct {
	Codecs     []string `yaml:"codecs"`
	JitterMs   int      `yaml:"jitter_ms"`
	SilenceRMS int      `yaml:"silence_rms"`
}

type NAT struct {
	Mode        string   `yaml:"mode"` // auto | local | stun | public_ip
	StunServers []string `yaml:"stun_servers"`
	PublicIP    string   `yaml:"public_ip"`
	RTPBind     string   `yaml:"rtp_bind"`
	RTPPortMin  int      `yaml:"rtp_port_min"`
	RTPPortMax  int      `yaml:"rtp_port_max"`
}

type Calls struct {
	CallerIDWaitMs      int `yaml:"caller_id_wait_ms"`
	SCOSettleMs         int `yaml:"sco_settle_ms"`
	MediaTimeoutSeconds int `yaml:"media_timeout_seconds"`
	MaxDurationSeconds  int `yaml:"max_duration_seconds"`
}

type Control struct {
	Socket string `yaml:"socket"`
}

type Hook struct {
	ID             string   `yaml:"id"`
	Events         []string `yaml:"events"`
	Command        string   `yaml:"command"`
	TimeoutSeconds int      `yaml:"timeout_seconds"`
}

type Config struct {
	Bluetooth Bluetooth `yaml:"bluetooth"`
	Phone     Phone     `yaml:"phone"`
	SIP       SIP       `yaml:"sip"`
	Media     Media     `yaml:"media"`
	NAT       NAT       `yaml:"nat"`
	Calls     Calls     `yaml:"calls"`
	Control   Control   `yaml:"control"`
	Hooks     []Hook    `yaml:"hooks"`
}

// Default returns the configuration with every default from spec section 9 applied.
// bluetooth.phone_address, sip.target (and phone.number when the target uses {number})
// have no usable default; Validate reports them.
func Default() Config {
	return Config{
		Bluetooth: Bluetooth{Adapter: "hci0", PowerCycleMinutes: 60},
		SIP: SIP{Transport: "tls", DisplayName: "{caller}", AnonymousUser: "anonymous",
			RegisterExpiresSeconds: 120, DialTimeoutSeconds: 30},
		Media: Media{Codecs: []string{"PCMU", "PCMA"}, JitterMs: 60, SilenceRMS: 200},
		NAT: NAT{Mode: "auto", StunServers: []string{"stun.l.google.com:19302", "stun.cloudflare.com:3478"},
			RTPPortMin: 10000, RTPPortMax: 20000},
		Calls:   Calls{CallerIDWaitMs: 1000, SCOSettleMs: 800, MediaTimeoutSeconds: 10},
		Control: Control{Socket: "siptooth.sock"},
	}
}

// targetHost returns the host of a sip: URI template (no user, port, parameters or
// headers); brackets of an IPv6 literal are kept. It returns "" when none is found.
func targetHost(uri string) string {
	rest := uri
	if len(rest) >= 4 && strings.EqualFold(rest[:4], "sip:") {
		rest = rest[4:]
	}
	if at := strings.LastIndexByte(rest, '@'); at >= 0 {
		rest = rest[at+1:]
	}
	if i := strings.IndexAny(rest, ";?"); i >= 0 {
		rest = rest[:i]
	}
	if strings.HasPrefix(rest, "[") {
		if end := strings.IndexByte(rest, ']'); end >= 0 {
			return rest[:end+1]
		}
		return rest
	}
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// FromTemplate is the From URI template: sip.from, or sip:{caller}@<target host> when unset.
func (s SIP) FromTemplate() string {
	if s.From != "" {
		return s.From
	}
	return "sip:{caller}@" + targetHost(s.Target)
}

// RegistrarHost is sip.registrar, or the target host when unset.
func (s SIP) RegistrarHost() string {
	if s.Registrar != "" {
		return s.Registrar
	}
	return targetHost(s.Target)
}

// decode reads strict YAML (unknown keys are errors) over the defaults.
func decode(r io.Reader) (Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && err != io.EOF {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	for i := range cfg.Hooks {
		if cfg.Hooks[i].TimeoutSeconds == 0 {
			cfg.Hooks[i].TimeoutSeconds = DefaultHookTimeoutSeconds
		}
	}
	return cfg, nil
}

// Load reads the file at path over the defaults (unknown keys are errors), applies the
// hook timeout default and resolves relative paths against the directory holding the file.
// It does not validate: call Validate with the password from LookupPassword.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	cfg, err := decode(f)
	if err != nil {
		return Config{}, err
	}
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return Config{}, fmt.Errorf("config directory: %w", err)
	}
	cfg.ResolvePaths(dir)
	return cfg, nil
}

// ResolvePaths makes the relative file system paths of c absolute against base.
func (c *Config) ResolvePaths(base string) {
	if p := &c.Control.Socket; *p != "" && !filepath.IsAbs(*p) {
		*p = filepath.Join(base, *p)
	}
}
