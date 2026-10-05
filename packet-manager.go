package sftp

import (
	"encoding"
	"sort"
	"sync"
)

// The goal of the packetManager is to keep the outgoing packets in the same
// order as the incoming as is requires by section 7 of the RFC.

type packetManager struct {
	requests    chan orderedPacket
	responses   chan orderedPacket
	fini        chan struct{}
	incoming    orderedPackets
	outgoing    orderedPackets
	sender      packetSender // connection object
	working     *sync.WaitGroup
	packetCount uint32
	// pendingIO tracks read and write requests that are still executing,
	// keyed by order ID. Each entry records the file handle the request
	// targets and whether it is a write: fsync barriers only wait on prior
	// writes to the same handle, while close waits on all prior requests to
	// the same handle. Closing done unblocks anything waiting on it.
	pendingWritesMu sync.Mutex
	pendingIO       map[uint32]pendingIO
	// it is not nil if the allocator is enabled
	alloc *allocator
}

type packetSender interface {
	sendPacket(encoding.BinaryMarshaler) error
}

func newPktMgr(sender packetSender) *packetManager {
	s := &packetManager{
		requests:  make(chan orderedPacket, SftpServerWorkerCount),
		responses: make(chan orderedPacket, SftpServerWorkerCount),
		fini:      make(chan struct{}),
		incoming:  make([]orderedPacket, 0, SftpServerWorkerCount),
		outgoing:  make([]orderedPacket, 0, SftpServerWorkerCount),
		sender:    sender,
		working:   &sync.WaitGroup{},
		pendingIO: make(map[uint32]pendingIO),
	}
	go s.controller()
	return s
}

// // packet ordering
func (s *packetManager) newOrderID() uint32 {
	s.packetCount++
	return s.packetCount
}

// returns the next orderID without incrementing it.
// This is used before receiving a new packet, with the allocator enabled, to associate
// the slice allocated for the received packet with the orderID that will be used to mark
// the allocated slices for reuse once the request is served
func (s *packetManager) getNextOrderID() uint32 {
	return s.packetCount + 1
}

type orderedRequest struct {
	requestPacket
	orderid uint32
}

func (s *packetManager) newOrderedRequest(p requestPacket) orderedRequest {
	return orderedRequest{requestPacket: p, orderid: s.newOrderID()}
}
func (p orderedRequest) orderID() uint32 { return p.orderid }

type orderedResponse struct {
	responsePacket
	orderid uint32
}

func (s *packetManager) newOrderedResponse(p responsePacket, id uint32,
) orderedResponse {
	return orderedResponse{responsePacket: p, orderid: id}
}
func (p orderedResponse) orderID() uint32 { return p.orderid }

type orderedPacket interface {
	id() uint32
	orderID() uint32
}
type orderedPackets []orderedPacket

func (o orderedPackets) Sort() {
	sort.Slice(o, func(i, j int) bool {
		return o[i].orderID() < o[j].orderID()
	})
}

// // packet registry
// register incoming packets to be handled
func (s *packetManager) incomingPacket(pkt orderedRequest) {
	s.working.Add(1)
	s.requests <- pkt
}

type pendingIO struct {
	handle string
	write  bool
	done   chan struct{}
}

// registerIO records that the request with the given order ID is in flight
// against handle. It must be called before the request is handed to a
// worker. The returned completion function unblocks any barrier waiting on
// this request, and must be called once it has finished (whether it
// succeeded or failed).
func (s *packetManager) registerIO(orderID uint32, handle string, write bool) (complete func()) {
	done := make(chan struct{})

	s.pendingWritesMu.Lock()
	s.pendingIO[orderID] = pendingIO{handle: handle, write: write, done: done}
	s.pendingWritesMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.pendingWritesMu.Lock()
			delete(s.pendingIO, orderID)
			s.pendingWritesMu.Unlock()
			close(done)
		})
	}
}

// waitForPriorIO blocks until every read or write request received before
// the request with barrierOrderID and targeting handle has completed.
func (s *packetManager) waitForPriorIO(barrierOrderID uint32, handle string, writesOnly bool) {
	s.pendingWritesMu.Lock()
	var pending []chan struct{}
	for orderID, io := range s.pendingIO {
		if orderID < barrierOrderID && io.handle == handle && (!writesOnly || io.write) {
			pending = append(pending, io.done)
		}
	}
	s.pendingWritesMu.Unlock()

	for _, done := range pending {
		<-done
	}
}

// waitForPriorWrites blocks until every write request received before the
// fsync request with fsyncOrderID and targeting handle has completed.
//
// The set of writes waited on is snapshotted at dispatch time: it contains
// only writes with lower order IDs, so a worker handling fsync before an
// earlier-dispatched write cannot pass the barrier, while a write dispatched
// after the fsync does not hold the barrier up.
func (s *packetManager) waitForPriorWrites(fsyncOrderID uint32, handle string) {
	s.waitForPriorIO(fsyncOrderID, handle, true)
}

// register outgoing packets as being ready
func (s *packetManager) readyPacket(pkt orderedResponse) {
	s.responses <- pkt
	s.working.Done()
}

// shut down packetManager controller
func (s *packetManager) close() {
	// pause until current packets are processed
	s.working.Wait()
	close(s.fini)
}

// Passed a worker function, returns a channel for incoming packets.
// Keep process packet responses in the order they are received while
// maximizing throughput of file transfers.
func (s *packetManager) workerChan(runWorker func(chan orderedRequest),
) chan orderedRequest {
	// multiple workers for faster read/writes
	rwChan := make(chan orderedRequest, SftpServerWorkerCount)
	for i := 0; i < SftpServerWorkerCount; i++ {
		runWorker(rwChan)
	}

	// single worker to enforce sequential processing of everything else
	// The buffer lets the dispatcher keep reading and dispatching read/write
	// requests while an fsync barrier blocks the command worker, so requests
	// on other handles are not held up behind it.
	cmdChan := make(chan orderedRequest, SftpServerWorkerCount)
	runWorker(cmdChan)

	pktChan := make(chan orderedRequest, SftpServerWorkerCount)
	go func() {
		for pkt := range pktChan {
			// Extended requests are wrapped in sshFxpExtendedPacket;
			// unwrap just enough to classify the request for scheduling
			// while leaving the wrapper in place for the worker.
			requestPacket := pkt.requestPacket
			if epkt, ok := requestPacket.(*sshFxpExtendedPacket); ok && epkt.SpecificPacket != nil {
				requestPacket = epkt.SpecificPacket
			}

			switch rpkt := requestPacket.(type) {
			case *sshFxpReadPacket:
				rpkt.complete = s.registerIO(pkt.orderID(), rpkt.Handle, false)
				s.incomingPacket(pkt)
				rwChan <- pkt
				continue
			case *sshFxpWritePacket:
				rpkt.complete = s.registerIO(pkt.orderID(), rpkt.Handle, true)
				s.incomingPacket(pkt)
				rwChan <- pkt
				continue
			case *sshFxpExtendedPacketFsync:
				// Snapshot the writes this barrier must wait on before the
				// request is visible to the command worker.
				orderID, handle := pkt.orderID(), rpkt.Handle
				rpkt.wait = func() { s.waitForPriorIO(orderID, handle, true) }
				s.incomingPacket(pkt)
				cmdChan <- pkt
				continue
			case *sshFxpClosePacket:
				// Wait for earlier reads and writes against this handle to
				// finish before closing it. Requests on other handles keep
				// running.
				orderID, handle := pkt.orderID(), rpkt.Handle
				rpkt.wait = func() { s.waitForPriorIO(orderID, handle, false) }
			}
			s.incomingPacket(pkt)
			// all non-RW use sequential cmdChan
			cmdChan <- pkt
		}
		close(rwChan)
		close(cmdChan)
		s.close()
	}()

	return pktChan
}

// process packets
func (s *packetManager) controller() {
	for {
		select {
		case pkt := <-s.requests:
			debug("incoming id (oid): %v (%v)", pkt.id(), pkt.orderID())
			s.incoming = append(s.incoming, pkt)
			s.incoming.Sort()
		case pkt := <-s.responses:
			debug("outgoing id (oid): %v (%v)", pkt.id(), pkt.orderID())
			s.outgoing = append(s.outgoing, pkt)
			s.outgoing.Sort()
		case <-s.fini:
			return
		}
		s.maybeSendPackets()
	}
}

// send as many packets as are ready
func (s *packetManager) maybeSendPackets() {
	for {
		if len(s.outgoing) == 0 || len(s.incoming) == 0 {
			debug("break! -- outgoing: %v; incoming: %v",
				len(s.outgoing), len(s.incoming))
			break
		}
		out := s.outgoing[0]
		in := s.incoming[0]
		// debug("incoming: %v", ids(s.incoming))
		// debug("outgoing: %v", ids(s.outgoing))
		if in.orderID() == out.orderID() {
			debug("Sending packet: %v", out.id())
			s.sender.sendPacket(out.(encoding.BinaryMarshaler))
			if s.alloc != nil {
				// mark for reuse the slices allocated for this request
				s.alloc.ReleasePages(in.orderID())
			}
			// pop off heads
			copy(s.incoming, s.incoming[1:])            // shift left
			s.incoming[len(s.incoming)-1] = nil         // clear last
			s.incoming = s.incoming[:len(s.incoming)-1] // remove last
			copy(s.outgoing, s.outgoing[1:])            // shift left
			s.outgoing[len(s.outgoing)-1] = nil         // clear last
			s.outgoing = s.outgoing[:len(s.outgoing)-1] // remove last
		} else {
			break
		}
	}
}

// func oids(o []orderedPacket) []uint32 {
// 	res := make([]uint32, 0, len(o))
// 	for _, v := range o {
// 		res = append(res, v.orderId())
// 	}
// 	return res
// }
// func ids(o []orderedPacket) []uint32 {
// 	res := make([]uint32, 0, len(o))
// 	for _, v := range o {
// 		res = append(res, v.id())
// 	}
// 	return res
// }
