package nbd

import (
	"fmt"
	"time"

	nbd "github.com/marmos91/dittofs/internal/adapter/nbd"
)

// DefaultPort is the port the NBD adapter binds when none is configured. It is
// the port the kernel NBD client's documentation uses for a userspace server;
// the protocol itself has no assigned port.
const DefaultPort = 10809

// DefaultMaxMessageSize is the default cap on a single NBD request payload.
// The wire layer's maximum block size is 32 MiB (the largest the adapter
// advertises in its block-size information reply), so anything larger than
// that is a client that ignored the advertisement.
const DefaultMaxMessageSize = 32 << 20

// DefaultMaxConnections is the cap applied when the operator configures none.
//
// It is deliberately much lower than pkg/adapter/nfs's 1024: an NBD connection
// holds a block-store handle and can be holding up to a 32 MiB read buffer,
// where an NFS connection holds neither. The NFS precedent's reasoning applies
// unchanged, though — "a sane default cap so the accept loop's connSemaphore is
// always built. Leaving this at 0 (unlimited) lets unauthenticated clients
// exhaust memory/goroutines" — and it applies harder here, because NBD has no
// authentication at all.
const DefaultMaxConnections = 64

// DefaultUID and DefaultGID are the unprivileged identity the adapter runs
// metadata operations as when the operator configures none: nobody/nogroup.
// NBD carries no user identity at all, so every operation runs as this one
// synthetic identity, and UID 0 would hand every client root on the volume.
const (
	DefaultUID = uint32(65534)
	DefaultGID = uint32(65534)
)

// TimeoutsConfig groups the operator-facing timers for an NBD connection.
type TimeoutsConfig struct {
	// Read is the maximum duration for finishing a request that has already
	// started: once a request header has been parsed, this bounds reading its
	// payload. The wait for a header is bounded by Idle instead, because a
	// client that has sent nothing is idle rather than slow.
	Read time.Duration `mapstructure:"read"`

	// Write is the maximum duration for writing a reply to a client. It
	// bounds a client that stops reading.
	Write time.Duration `mapstructure:"write"`

	// Idle is the maximum duration a connection may remain idle between
	// requests before the adapter closes it. It bounds the wait for a request
	// header, so a client that stops part-way through one is also covered. 0
	// means no idle timeout.
	Idle time.Duration `mapstructure:"idle"`

	// Shutdown is the maximum duration to wait for in-flight connections to
	// finish during graceful shutdown. It is handed to the base adapter, which
	// owns the drain deadline.
	Shutdown time.Duration `mapstructure:"shutdown"`
}

// Config holds the operator-facing configuration for the NBD adapter.
//
// Default values (applied by New when zero):
//   - Port: 10809
//   - Timeouts.Read: 5m
//   - Timeouts.Write: 30s
//   - Timeouts.Idle: 5m
//   - Timeouts.Shutdown: 30s
//   - MaxMessageSize: 32 MiB
//   - MaxConnections: 64
//   - UID/GID: 65534 (nobody/nogroup)
type Config struct {
	// Enabled controls whether the adapter is active. When false the adapter
	// is not started and reports StatusDisabled.
	Enabled bool `mapstructure:"enabled"`

	// Port is the TCP port to listen on. 0 means the default (10809).
	Port int `mapstructure:"port"`

	// BindAddress is the interface to bind. An empty string or "0.0.0.0"
	// binds every interface, which for an unauthenticated protocol requires
	// AllowRemoteBind.
	BindAddress string `mapstructure:"bind_address"`

	// MaxConnections limits concurrent client connections. 0 means the default
	// (64); -1 means unlimited, which validate refuses together with
	// AllowRemoteBind because an uncapped, unauthenticated, remotely reachable
	// adapter is one accept loop away from exhausting file descriptors.
	MaxConnections int `mapstructure:"max_connections"`

	// MaxMessageSize caps a single request payload in bytes. 0 means the
	// default (32 MiB).
	MaxMessageSize int `mapstructure:"max_message_size"`

	// Timeouts groups the connection timers.
	Timeouts TimeoutsConfig `mapstructure:"timeouts"`

	// AllowRemoteBind permits binding an interface reachable off-host. NBD has
	// no authentication and no per-path scoping — a single READ returns every
	// byte of the volume — so a remote bind is off by default and an operator
	// must opt in explicitly.
	AllowRemoteBind bool `mapstructure:"allow_remote_bind"`

	// DefaultExport is the export name offered to a client that requests none.
	// An export name is of the form <share>/<volume>.
	DefaultExport string `mapstructure:"default_export"`

	// UID is the Unix UID every metadata operation runs as. NBD carries no
	// client identity, so this is the sole principal.
	UID uint32 `mapstructure:"uid"`

	// GID is the Unix GID every metadata operation runs as.
	GID uint32 `mapstructure:"gid"`
}

// applyDefaults fills zero values with the adapter's defaults.
func (c *Config) applyDefaults() {
	if c.Port <= 0 {
		c.Port = DefaultPort
	}
	if c.Timeouts.Read == 0 {
		c.Timeouts.Read = 5 * time.Minute
	}
	if c.Timeouts.Write == 0 {
		c.Timeouts.Write = 30 * time.Second
	}
	if c.Timeouts.Idle == 0 {
		c.Timeouts.Idle = 5 * time.Minute
	}
	if c.Timeouts.Shutdown == 0 {
		c.Timeouts.Shutdown = 30 * time.Second
	}
	if c.MaxMessageSize == 0 {
		c.MaxMessageSize = DefaultMaxMessageSize
	}
	// 0 is no longer "unlimited". The zero value of an omitted key used to mean
	// an uncapped, unauthenticated adapter, and pkg/adapter/base.go:159 only
	// allocates its semaphore when the value is positive — so the default
	// configuration had no limit at all. -1 is the explicit spelling for
	// unlimited, which makes it a decision an operator writes down.
	if c.MaxConnections == 0 {
		c.MaxConnections = DefaultMaxConnections
	}
	if c.UID == 0 {
		c.UID = DefaultUID
	}
	if c.GID == 0 {
		c.GID = DefaultGID
	}
}

// validate reports whether the configuration describes an adapter that can be
// built. It has exactly one caller, New, which runs applyDefaults immediately
// before it, so a port here is always in range or too large: applyDefaults
// rewrites anything <= 0 to DefaultPort, which makes a negative port
// unreachable rather than merely unlikely.
func (c *Config) validate() error {
	if c.Port > 65535 {
		return fmt.Errorf("invalid port %d: must be 0-65535", c.Port)
	}
	// A remote bind with no cap is the one combination worth refusing rather
	// than defaulting, because the operator asked for both halves explicitly:
	// -1 is the deliberate spelling of "unlimited", and pairing it with a
	// reachable interface is a decision the adapter should not make silently.
	//
	// This reaches the operator as a panic from New, which is the same path every
	// other invalid config takes. models.AdapterConfig.Validate
	// (pkg/controlplane/models/adapter.go:55-77) checks only the type and the
	// port, so it does not catch this first — this is the gate.
	if c.AllowRemoteBind && c.MaxConnections < 0 {
		return fmt.Errorf("allow_remote_bind requires a connection limit: set max_connections to a positive value")
	}
	// Any other negative value is a typo, not a policy.
	if c.MaxConnections < -1 {
		return fmt.Errorf("invalid max_connections %d: use 0 for the default, -1 for unlimited, or a positive limit", c.MaxConnections)
	}
	return nil
}

// timeouts narrows the adapter's four configured durations to the three the
// wire layer needs. Shutdown is deliberately not here: the base adapter owns
// the drain deadline, and the handler's job during drain is to answer
// ErrESHUTDOWN, not to run its own timer.
func (c *Config) timeouts() nbd.Timeouts {
	return nbd.Timeouts{Read: c.Timeouts.Read, Write: c.Timeouts.Write, Idle: c.Timeouts.Idle}
}
