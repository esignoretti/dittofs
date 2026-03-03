package smb

import (
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
)

// DispatchHook is called before or after handler execution for a specific command.
// It receives the connection info, the command being dispatched, and the raw SMB2
// message bytes (header + body) for that message.
//
// Hooks are used for cross-cutting concerns that need access to raw wire bytes,
// such as the preauth integrity hash chain computation for SMB 3.1.1.
type DispatchHook func(connInfo *ConnInfo, command types.Command, rawMessage []byte)

var (
	// beforeHooks are invoked before the handler processes the request.
	// Key: command code, Value: slice of hooks to run in order.
	beforeHooks map[types.Command][]DispatchHook

	// afterHooks are invoked after the handler has produced a response.
	// Key: command code, Value: slice of hooks to run in order.
	afterHooks map[types.Command][]DispatchHook
)

func init() {
	beforeHooks = make(map[types.Command][]DispatchHook)
	afterHooks = make(map[types.Command][]DispatchHook)

	// Register preauth integrity hash hooks for NEGOTIATE and SESSION_SETUP.
	// Per [MS-SMB2] 3.3.5.4: the preauth integrity hash chain is updated
	// with the raw bytes of every NEGOTIATE and SESSION_SETUP message.
	//
	// Before-hook: always updates the hash with the request bytes.
	// The dialect is not yet known at request time, so we always hash.
	// If the negotiated dialect is not 3.1.1, the hash is simply unused.
	registerBeforeHook(types.CommandNegotiate, preauthHashBeforeHook)
	registerBeforeHook(types.CommandSessionSetup, preauthHashBeforeHook)

	// After-hook: updates the hash with the response bytes, but only if
	// the negotiated dialect is 3.1.1 (checked via CryptoState.Dialect).
	registerAfterHook(types.CommandNegotiate, preauthHashAfterHook)
	registerAfterHook(types.CommandSessionSetup, preauthHashAfterHook)
}

// registerBeforeHook appends a hook to run before handler execution for the given command.
// Must only be called during init().
func registerBeforeHook(cmd types.Command, hook DispatchHook) {
	beforeHooks[cmd] = append(beforeHooks[cmd], hook)
}

// registerAfterHook appends a hook to run after handler execution for the given command.
// Must only be called during init().
func registerAfterHook(cmd types.Command, hook DispatchHook) {
	afterHooks[cmd] = append(afterHooks[cmd], hook)
}

// RunBeforeHooks runs all before-hooks registered for the given command.
func RunBeforeHooks(connInfo *ConnInfo, cmd types.Command, rawMessage []byte) {
	for _, hook := range beforeHooks[cmd] {
		hook(connInfo, cmd, rawMessage)
	}
}

// RunAfterHooks runs all after-hooks registered for the given command.
func RunAfterHooks(connInfo *ConnInfo, cmd types.Command, rawMessage []byte) {
	for _, hook := range afterHooks[cmd] {
		hook(connInfo, cmd, rawMessage)
	}
}

// preauthHashBeforeHook updates the preauth integrity hash with request bytes.
// Called before the handler processes NEGOTIATE or SESSION_SETUP requests.
//
// Per [MS-SMB2] 3.3.5.4: H(i) = SHA-512(H(i-1) || Message(i))
// We always update here because the dialect is not yet known at NEGOTIATE time.
// If the final negotiated dialect is not 3.1.1, the hash value is simply unused.
//
// For SESSION_SETUP, this ensures the preauth hash includes the request bytes
// BEFORE the handler derives signing keys (critical for SMB 3.1.1 key derivation).
func preauthHashBeforeHook(connInfo *ConnInfo, cmd types.Command, rawMessage []byte) {
	if connInfo.CryptoState == nil {
		return
	}
	connInfo.CryptoState.UpdatePreauthHash(rawMessage)
	logger.Debug("Preauth hash updated with request",
		"command", cmd.String(),
		"messageLen", len(rawMessage))
}

// preauthHashAfterHook updates the preauth integrity hash with response bytes.
// Called after NEGOTIATE or SESSION_SETUP responses are sent.
// Only updates if the negotiated dialect is 3.1.1.
//
// Per [MS-SMB2] 3.3.5.4: The response hash is computed only when 3.1.1 is selected.
//
// For SESSION_SETUP, the after-hook runs AFTER the response is signed and sent,
// so the final SESSION_SETUP response hash does not affect the signing key used
// for that response (which is correct per spec — the key is derived from the
// hash up to and including the final SESSION_SETUP request).
func preauthHashAfterHook(connInfo *ConnInfo, cmd types.Command, rawMessage []byte) {
	if connInfo.CryptoState == nil {
		return
	}
	if connInfo.CryptoState.GetDialect() != types.Dialect0311 {
		return
	}
	connInfo.CryptoState.UpdatePreauthHash(rawMessage)
	logger.Debug("Preauth hash updated with response",
		"command", cmd.String(),
		"messageLen", len(rawMessage))
}
