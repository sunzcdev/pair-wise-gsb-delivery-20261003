package ftpserver

import (
	"fmt"
	"strings"
)

// pretSupportedCommands lists the transfer commands that can be announced with PRET.
var pretSupportedCommands = map[string]struct{}{ //nolint:gochecknoglobals
	"RETR": {},
	"STOR": {},
	"APPE": {},
	"LIST": {},
	"NLST": {},
	"MLSD": {},
}

// preTransferState tracks a reservation made through the PRET command.
type preTransferState struct {
	info        PreTransferInfo
	reservation TransferReservation
	consumed    bool
	released    bool
	cancel      func(PreTransferInfo, TransferReservation)
	release     func(PreTransferInfo, TransferReservation, error)
}

func (c *clientHandler) handlePRET(param string) error {
	ext, ok := c.driver.(ClientDriverExtensionPreTransfer)
	if !ok {
		c.writeMessage(StatusCommandNotImplemented, "PRET support is disabled")

		return nil
	}

	spl := strings.SplitN(strings.TrimSpace(param), " ", 2)
	command := strings.ToUpper(spl[0])

	if _, supported := pretSupportedCommands[command]; !supported {
		c.writeMessage(
			StatusSyntaxErrorParameters,
			fmt.Sprintf("PRET cannot be used with the %q command", command),
		)

		return nil
	}

	pathParam := ""
	if len(spl) == 2 {
		pathParam = spl[1]
	}

	// Only directory listings may be announced without an explicit path.
	if pathParam == "" && command != "LIST" && command != "NLST" && command != "MLSD" {
		c.writeMessage(StatusSyntaxErrorParameters, "PRET requires a path")

		return nil
	}

	info := PreTransferInfo{
		Command:      command,
		Path:         c.absPath(pathParam),
		Restart:      c.ctxRest,
		TransferType: c.currentTransferType,
	}

	// A new PRET supersedes a reservation that never got consumed. Releasing
	// it before preparing the new one lets the driver free resources first.
	c.transferMu.Lock()
	c.cancelPreTransferLocked()
	c.transferMu.Unlock()

	reservation, err := ext.PrepareTransfer(c, info)
	if err != nil {
		c.writeMessage(getErrorCode(err, StatusActionNotTaken), "Could not prepare transfer: "+err.Error())

		return nil
	}

	state := &preTransferState{
		info:        info,
		reservation: reservation,
	}

	if lifecycle, hasLifecycle := reservation.(TransferReservationLifecycle); hasLifecycle {
		state.cancel = lifecycle.Cancel
		state.release = lifecycle.Release
	}

	c.transferMu.Lock()
	c.preTransfer = state
	c.transferMu.Unlock()

	c.writeMessage(StatusFileActionPending, fmt.Sprintf("PRET command OK: %s", command))

	return nil
}

// consumePreTransfer validates the reservation against the transfer that is
// about to start. It returns false when the transfer must not proceed.
// On success the reservation is marked as consumed; it has to be released with
// releaseConsumedPreTransfer once the transfer attempt is over.
func (c *clientHandler) consumePreTransfer(command, pathParam string) bool {
	if _, ok := c.driver.(ClientDriverExtensionPreTransfer); !ok {
		return true
	}

	expected := PreTransferInfo{
		Command:      command,
		Path:         c.absPath(pathParam),
		Restart:      c.ctxRest,
		TransferType: c.currentTransferType,
	}

	c.transferMu.Lock()
	defer c.transferMu.Unlock()

	state := c.preTransfer

	if state == nil {
		c.writeMessage(
			StatusBadCommandSequence,
			fmt.Sprintf("%s requires a matching PRET command first", command),
		)

		return false
	}

	if state.consumed {
		c.cancelPreTransferLocked()
		c.writeMessage(StatusBadCommandSequence, "PRET reservation was already used")

		return false
	}

	if reason, mismatch := preTransferMismatch(state.info, expected); mismatch {
		c.cancelPreTransferLocked()
		c.writeMessage(
			StatusBadCommandSequence,
			fmt.Sprintf("PRET reservation cannot be reused: %s", reason),
		)

		return false
	}

	state.consumed = true

	if state.reservation != nil {
		state.reservation.Consumed()
	}

	return true
}

func preTransferMismatch(announced, expected PreTransferInfo) (string, bool) {
	switch {
	case announced.Command != expected.Command:
		return fmt.Sprintf("announced command %s, got %s", announced.Command, expected.Command), true
	case announced.Path != expected.Path:
		return fmt.Sprintf("announced path %s, got %s", announced.Path, expected.Path), true
	case announced.Restart != expected.Restart:
		return fmt.Sprintf("announced restart %d, got %d", announced.Restart, expected.Restart), true
	case announced.TransferType != expected.TransferType:
		return "transfer type changed", true
	default:
		return "", false
	}
}

// releaseConsumedPreTransfer releases a reservation that was consumed by a
// transfer. It is safe to call even when no reservation exists anymore.
func (c *clientHandler) releaseConsumedPreTransfer(err error) {
	c.transferMu.Lock()
	state := c.preTransfer
	if state == nil {
		c.transferMu.Unlock()

		return
	}

	c.preTransfer = nil
	c.transferMu.Unlock()

	if state.released {
		return
	}

	state.released = true

	if state.release != nil {
		state.release(state.info, state.reservation, err)
	}
}

// cancelPreTransferLocked invalidates a reservation that was never consumed
// (re-PRET, mismatch or client disconnect). The caller must hold transferMu.
func (c *clientHandler) cancelPreTransferLocked() {
	state := c.preTransfer
	if state == nil || state.consumed {
		// A consumed reservation is owned by the transfer goroutine, which is
		// responsible for releasing it once the transfer attempt ends.
		return
	}

	c.preTransfer = nil

	if state.released || state.cancel == nil {
		return
	}

	state.released = true
	state.cancel(state.info, state.reservation)
}
