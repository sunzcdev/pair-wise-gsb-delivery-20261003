// Copyright IBM Corp. 2026
// SPDX-License-Identifier: MPL-2.0

package raft

import (
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"
)

// addressMigrationPhase selects the requested migration operation.
type addressMigrationPhase uint8

const (
	migrationPrepare addressMigrationPhase = iota
	migrationCommit
	migrationAbort
)

// addressMigrationFuture is returned by PrepareServerAddress,
// CommitServerAddress and AbortServerAddress. It embeds a logFuture so that a
// commit can be tracked through the normal inflight-commit machinery.
type addressMigrationFuture struct {
	logFuture
}

// addressMigrationRequest is enqueued on Raft.addressMigrationCh.
type addressMigrationRequest struct {
	phase      addressMigrationPhase
	serverID   ServerID
	oldAddress ServerAddress
	newAddress ServerAddress
	future     *addressMigrationFuture
}

// addressMigration tracks a migration that has been prepared on the current
// leader but not yet committed.
type addressMigration struct {
	serverID   ServerID
	oldAddress ServerAddress
	newAddress ServerAddress

	// preparedIndex is the latest configuration index at prepare time. The
	// commit is rejected if the configuration moved on in the meantime.
	preparedIndex uint64

	// shadow replicates to the new address without contributing a vote or
	// replication progress, keeping the endpoint warm until the commit.
	shadow *followerReplication
}

// addressProbeResult carries the result of an asynchronous identity probe
// back to the leader's main loop.
type addressProbeResult struct {
	term uint64
	req  *addressMigrationRequest
	resp *VerifyServerResponse
	err  error
}

// prepareServerAddress runs on the leader main loop. It validates the request
// against the current configuration and starts an asynchronous identity probe
// against the new address.
func (r *Raft) prepareServerAddress(req *addressMigrationRequest) {
	current := r.configurations.latest

	server, err := migrationTarget(current, req.serverID)
	if err != nil {
		req.future.respond(err)
		return
	}
	if server.Suffrage != Voter {
		req.future.respond(ErrNotVoter)
		return
	}
	if req.oldAddress != server.Address {
		req.future.respond(ErrAddressMigrationStale)
		return
	}
	if req.newAddress == "" || req.newAddress == req.oldAddress {
		req.future.respond(ErrAddressMigrationMismatch)
		return
	}
	if addrOwner(current, req.newAddress) != "" {
		r.logger.Warn("cannot migrate to address already in use",
			"server-id", req.serverID, "new-address", req.newAddress)
		req.future.respond(ErrAddressMigrationMismatch)
		return
	}

	// A re-prepare of the identical target is idempotent; any other prepared
	// migration for the same server must be committed or aborted first.
	if existing, ok := r.leaderState.migrations[req.serverID]; ok {
		if existing.newAddress != req.newAddress || existing.oldAddress != req.oldAddress {
			req.future.respond(ErrAddressMigrationInProgress)
			return
		}
		r.abortPreparedMigration(existing)
	}

	prober, ok := r.trans.(WithServerProbe)
	if !ok {
		req.future.respond(ErrProbeUnsupported)
		return
	}

	probeReq := &VerifyServerRequest{RPCHeader: r.getRPCHeader()}
	probeResp := &VerifyServerResponse{}

	r.logger.Info("preparing server address migration",
		"server-id", req.serverID,
		"old-address", req.oldAddress,
		"new-address", req.newAddress)

	go func() {
		err := prober.VerifyServer(req.serverID, req.newAddress, probeReq, probeResp)
		result := &addressProbeResult{
			term: r.getCurrentTerm(),
			req:  req,
			resp: probeResp,
			err:  err,
		}
		select {
		case r.addressProbeCh <- result:
		case <-r.shutdownCh:
			req.future.respond(ErrRaftShutdown)
		}
	}()
}

// handleAddressProbe runs on the leader main loop once an identity probe
// completes.
func (r *Raft) handleAddressProbe(result *addressProbeResult) {
	req := result.req

	// The probe was started in a previous term (for example because
	// leadership changed while it was in flight); its result is stale.
	if r.getState() != Leader || result.term != r.getCurrentTerm() {
		req.future.respond(ErrNotLeader)
		return
	}

	if result.err != nil {
		r.logger.Warn("server address probe failed",
			"server-id", req.serverID,
			"new-address", req.newAddress,
			"error", result.err)
		req.future.respond(result.err)
		return
	}

	respID := ServerID(result.resp.ID)
	if respID != req.serverID {
		r.logger.Warn("server identity mismatch at new address",
			"server-id", req.serverID,
			"new-address", req.newAddress,
			"responded-id", respID)
		req.future.respond(ErrAddressMigrationMismatch)
		return
	}

	// A higher term means the endpoint has participated in a newer election;
	// this leader is stale and must step down. The caller can retry against
	// the new leader.
	if result.resp.Term > r.getCurrentTerm() {
		r.logger.Warn("probed server has a newer term, stepping down",
			"server-id", req.serverID,
			"new-address", req.newAddress,
			"term", result.resp.Term)
		req.future.respond(ErrNotLeader)
		asyncNotifyCh(r.leaderState.stepDown)
		return
	}

	// The configuration may have changed while the probe was in flight.
	current := r.configurations.latest
	server, err := migrationTarget(current, req.serverID)
	if err != nil || server.Suffrage != Voter ||
		server.Address != req.oldAddress ||
		addrOwner(current, req.newAddress) != "" {
		req.future.respond(ErrAddressMigrationStale)
		return
	}

	// Identity is verified: start a shadow replication routine against the
	// new address so it is caught up by commit time without affecting the
	// cluster's voting or replication progress.
	shadow := r.newShadowReplication(server, req.newAddress)
	migration := &addressMigration{
		serverID:      req.serverID,
		oldAddress:    req.oldAddress,
		newAddress:    req.newAddress,
		preparedIndex: r.configurations.latestIndex,
		shadow:        shadow,
	}
	r.leaderState.migrations[req.serverID] = migration
	r.goFunc(func() { r.replicate(shadow) })
	asyncNotifyCh(shadow.triggerCh)

	r.logger.Info("server address migration prepared",
		"server-id", req.serverID,
		"old-address", req.oldAddress,
		"new-address", req.newAddress)

	req.future.respond(nil)
}

// commitServerAddress runs on the leader main loop and switches the official
// address of a prepared migration.
func (r *Raft) commitServerAddress(request *addressMigrationRequest) {
	req := request.future
	migration, ok := r.leaderState.migrations[request.serverID]
	if !ok {
		req.respond(ErrAddressMigrationNotPrepared)
		return
	}
	if migration.newAddress != request.newAddress ||
		migration.oldAddress != request.oldAddress {
		req.respond(ErrAddressMigrationNotPrepared)
		return
	}

	// Re-validate against the live configuration before committing.
	current := r.configurations.latest
	server, err := migrationTarget(current, request.serverID)
	if err != nil || server.Suffrage != Voter ||
		server.Address != request.oldAddress {
		r.abortPreparedMigration(migration)
		req.respond(ErrAddressMigrationStale)
		return
	}
	if migration.preparedIndex != r.configurations.latestIndex ||
		addrOwner(current, request.newAddress) != "" {
		r.abortPreparedMigration(migration)
		req.respond(ErrAddressMigrationStale)
		return
	}

	// Generate the new configuration (same semantics as AddVoter with the
	// new address) and append it as a standard configuration entry.
	configuration, err := nextConfiguration(current, r.configurations.latestIndex, configurationChangeRequest{
		command:       AddVoter,
		serverID:      request.serverID,
		serverAddress: request.newAddress,
	})
	if err != nil {
		r.abortPreparedMigration(migration)
		r.logger.Warn("failed to build migrated configuration", "error", err)
		req.respond(err)
		return
	}

	// Capture the shadow's progress and stop it. The shadow never
	// contributed to commitment; its next index simply seeds the new
	// official replication routine.
	shadowNext := atomic.LoadUint64(&migration.shadow.nextIndex)
	r.stopMigrationShadow(migration)

	// Detach the replication routine bound to the old address. Any response
	// still in flight over an old connection will be ignored, so it can
	// neither count twice nor move the follower's match index. Removing it
	// from replState means startStopReplication() creates a fresh routine
	// for the new address below.
	if old, ok := r.leaderState.replState[request.serverID]; ok {
		old.detached.Store(true)
		delete(r.leaderState.replState, request.serverID)
		close(old.stopCh)
	}
	delete(r.leaderState.migrations, request.serverID)

	r.logger.Info("committing server address migration",
		"server-id", request.serverID,
		"old-address", request.oldAddress,
		"new-address", request.newAddress,
		"servers", hclog.Fmt("%+v", configuration.Servers))

	req.log = Log{
		Type: LogConfiguration,
		Data: EncodeConfiguration(configuration),
	}
	r.dispatchLogs([]*logFuture{&req.logFuture})

	index := req.Index()
	r.setLatestConfiguration(configuration, index)
	r.leaderState.commitment.setConfiguration(configuration)

	// Seed the new routine with the shadow's progress, then (re)start
	// replication so the new address is used.
	if shadowNext > 0 && request.serverID != r.localID {
		r.leaderState.replicationHints[request.serverID] = shadowNext
	}
	r.startStopReplication()

	// Best effort: drop idle pooled connections to the old address so a
	// future RPC cannot be silently sent to the stale endpoint.
	if evictor, ok := r.trans.(WithConnectionPoolEviction); ok {
		evictor.EvictPeer(request.oldAddress)
	}
}

// abortServerAddress runs on the leader main loop.
func (r *Raft) abortServerAddress(request *addressMigrationRequest) {
	migration, ok := r.leaderState.migrations[request.serverID]
	if !ok {
		request.future.respond(nil)
		return
	}
	r.abortPreparedMigration(migration)
	request.future.respond(nil)
}

// abortPreparedMigration stops a prepared shadow and removes the migration.
func (r *Raft) abortPreparedMigration(migration *addressMigration) {
	r.stopMigrationShadow(migration)
	delete(r.leaderState.migrations, migration.serverID)
	r.logger.Info("aborted server address migration",
		"server-id", migration.serverID,
		"new-address", migration.newAddress)
}

// stopMigrationShadow detaches and stops the shadow replication routine.
func (r *Raft) stopMigrationShadow(migration *addressMigration) {
	if migration.shadow == nil {
		return
	}
	migration.shadow.detached.Store(true)
	select {
	case <-migration.shadow.stopCh:
		// Already closed.
	default:
		close(migration.shadow.stopCh)
	}
	migration.shadow = nil
}

// stopAllMigrationShadows detaches and stops every prepared shadow. It is
// invoked from the leader state cleanup when leadership is lost.
func (r *Raft) stopAllMigrationShadows() {
	for _, migration := range r.leaderState.migrations {
		r.stopMigrationShadow(migration)
	}
	r.leaderState.migrations = nil
}

// newShadowReplication builds a probe replication state for the new address.
func (r *Raft) newShadowReplication(peer Server, newAddress ServerAddress) *followerReplication {
	shadowPeer := peer
	shadowPeer.Address = newAddress
	return &followerReplication{
		peer:                shadowPeer,
		commitment:          r.leaderState.commitment,
		probe:               true,
		stopCh:              make(chan uint64, 1),
		triggerCh:           make(chan struct{}, 1),
		triggerDeferErrorCh: make(chan *deferError, 1),
		currentTerm:         r.getCurrentTerm(),
		nextIndex:           r.getLastIndex() + 1,
		lastContact:         time.Now(),
		notify:              make(map[*verifyFuture]struct{}),
		notifyCh:            make(chan struct{}, 1),
		stepDown:            r.leaderState.stepDown,
	}
}

// migrationTarget returns the configured server for the given ID.
func migrationTarget(configuration Configuration, id ServerID) (Server, error) {
	for _, server := range configuration.Servers {
		if server.ID == id {
			return server, nil
		}
	}
	return Server{}, ErrAddressMigrationNotPrepared
}

// addrOwner returns the ID of the configured server using the address, or "".
func addrOwner(configuration Configuration, address ServerAddress) ServerID {
	for _, server := range configuration.Servers {
		if server.Address == address {
			return server.ID
		}
	}
	return ""
}
