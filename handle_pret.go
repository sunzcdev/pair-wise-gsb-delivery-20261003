package ftpserver

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

var (
	errPRETNotImplemented = errors.New("PRET support hasn't been implemented")
	errPRETBadCommand     = errors.New("PRET only announces a transfer command")
)

// pretCommands lists the transfer commands that can be announced with PRET.
var pretCommands = map[string]struct{}{ //nolint:gochecknoglobals
	"RETR": {},
	"STOR": {},
	"APPE": {},
	"LIST": {},
	"NLST": {},
	"MLSD": {},
}

// directoryCommands are the PRET-able commands that list directories and that
// accept an optional path.
var directoryCommands = map[string]struct{}{ //nolint:gochecknoglobals
	"LIST": {},
	"NLST": {},
	"MLSD": {},
}

// transferFinalizer ends the lifecycle of a claimed PRET reservation. commit is
// called once the transfer connection opens successfully (the reserved
// resources are consumed), otherwise release is called.
type transferFinalizer struct {
	once     sync.Once
	prepared PreparedTransfer
}

func (f *transferFinalizer) commit() {
	if f == nil || f.prepared == nil {
		return
	}

	f.once.Do(f.prepared.Commit)
}

func (f *transferFinalizer) release() {
	if f == nil || f.prepared == nil {
		return
	}

	f.once.Do(f.prepared.Release)
}

func (c *clientHandler) handlePRET(param string) error {
	preparer, ok := c.driver.(ClientDriverExtensionPreTransfer)
	if !ok {
		c.writeMessage(StatusCommandNotImplemented, errPRETNotImplemented.Error())

		return nil
	}

	plan, err := c.parsePRETPlan(param)
	if err != nil {
		c.writeMessage(StatusSyntaxErrorParameters, err.Error())

		return nil
	}

	// A new announcement supersedes any previous one: free the old resources
	// before asking the driver to prepare the new transfer.
	c.releasePendingTransfer()

	prepared, err := preparer.PrepareTransfer(plan)
	if err != nil {
		c.writeMessage(StatusActionNotTaken, fmt.Sprintf("Couldn't prepare %s %s: %v",
			plan.Command, plan.Path, err))

		return nil
	}

	c.setPendingTransfer(prepared, plan)
	c.writeMessage(StatusOK, fmt.Sprintf("Prepared %s %s", plan.Command, plan.Path))

	return nil
}

func (c *clientHandler) parsePRETPlan(param string) (*TransferPlan, error) {
	command, pathParam := parseLine(param)
	command = strings.ToUpper(command)

	if _, ok := pretCommands[command]; !ok {
		return nil, fmt.Errorf("%w: %q", errPRETBadCommand, command)
	}

	// File transfer commands always carry a path, directory listings are
	// allowed to target the current directory implicitly.
	_, isDirectory := directoryCommands[command]

	if pathParam == "" && !isDirectory {
		return nil, fmt.Errorf("%w: missing path for %s", errPRETBadCommand, command)
	}

	if isDirectory && (command == "LIST" || command == "NLST") &&
		!c.server.settings.DisableLISTArgs {
		pathParam = c.checkLISTArgs(pathParam)
	}

	plan := &TransferPlan{
		Command:      command,
		Path:         c.absPath(pathParam),
		TransferType: c.currentTransferType,
	}

	// REST has no meaning for APPEND and is ignored there, mirroring
	// transferFile.
	if command != "APPE" {
		plan.RestartOffset = c.ctxRest
	}

	return plan, nil
}

// setPendingTransfer stores the resources reserved for an announced transfer.
func (c *clientHandler) setPendingTransfer(prepared PreparedTransfer, plan *TransferPlan) {
	c.transferMu.Lock()
	defer c.transferMu.Unlock()

	c.pendingTransfer = prepared
	c.pendingPlan = plan
}

// takePendingTransfer detaches the pending reservation and reports whether the
// incoming command matches what was announced. present is false when there is
// no pending reservation. matched is false for a different command, path,
// transfer type or REST offset. The detached reservation is always returned
// when present so the caller can release it.
func (c *clientHandler) takePendingTransfer(plan *TransferPlan) (present bool, matched bool,
	prepared PreparedTransfer,
) {
	c.transferMu.Lock()

	prepared = c.pendingTransfer
	pendingPlan := c.pendingPlan
	c.pendingTransfer = nil
	c.pendingPlan = nil

	c.transferMu.Unlock()

	if prepared == nil {
		return false, false, nil
	}

	matched = pendingPlan.Command == plan.Command &&
		pendingPlan.Path == plan.Path &&
		pendingPlan.TransferType == plan.TransferType &&
		pendingPlan.RestartOffset == plan.RestartOffset

	return true, matched, prepared
}

// releasePendingTransfer frees the pending reservation, if any.
func (c *clientHandler) releasePendingTransfer() {
	c.transferMu.Lock()
	prepared := c.pendingTransfer
	pendingPlan := c.pendingPlan
	c.pendingTransfer = nil
	c.pendingPlan = nil
	c.transferMu.Unlock()

	if prepared != nil {
		c.logger.Debug("Releasing pending PRET transfer",
			"command", pendingPlan.Command, "path", pendingPlan.Path)
		prepared.Release()
	}
}

// invalidatePendingTransfer releases the pending reservation because the
// context it was prepared for (REST offset or transfer type) changed.
func (c *clientHandler) invalidatePendingTransfer() {
	c.releasePendingTransfer()
}

// claimTransfer consumes the pending reservation for an incoming transfer
// command. It must be called before any driver filesystem access.
//
// When there is no pending reservation the transfer proceeds unconstrained.
// When the command matches the announcement, the returned finalizer commits
// the reservation once the transfer connection opens and releases it
// beforehand on failure. When the command does not match the announcement the
// reservation is released, a 503 reply is sent and ok is false so the caller
// aborts without touching the filesystem.
func (c *clientHandler) claimTransfer(plan *TransferPlan) (*transferFinalizer, bool) {
	present, matched, prepared := c.takePendingTransfer(plan)

	switch {
	case !present:
		return &transferFinalizer{}, true
	case !matched:
		prepared.Release()
		c.writeMessage(StatusBadCommandSequence,
			fmt.Sprintf("Transfer %s %s doesn't match the PRET announcement", plan.Command, plan.Path))

		return nil, false
	}

	return &transferFinalizer{prepared: prepared}, true
}

// claimDirectoryTransfer is the entry point of a directory listing command.
func (c *clientHandler) claimDirectoryTransfer(command, param string) (*transferFinalizer, bool) {
	if (command == "LIST" || command == "NLST") && !c.server.settings.DisableLISTArgs {
		param = c.checkLISTArgs(param)
	}

	plan := &TransferPlan{
		Command:      command,
		Path:         c.absPath(param),
		TransferType: c.currentTransferType,
	}

	return c.claimTransfer(plan)
}
