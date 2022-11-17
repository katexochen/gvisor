// Copyright 2022 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package stack

import (
	"fmt"
	"time"

	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// TODO(b/256037250): I still see the occasional SACK block in the zero-loss
// benchmark, which should not happen.
// TODO(b/256037250): Some dispatchers, e.g. XDP and RecvMmsg, can receive
// multiple packets at a time. Even if the GRO interval is 0, there is an
// opportunity for coalescing.
// TODO(b/256037250): We're doing some header parsing here, which presents the
// opportunity to skip it later.
// TODO(b/256037250): Disarm or ignore the timer when GRO is empty.
// TODO(b/256037250): We may be able to remove locking by pairing
// groDispatchers with link endpoint dispatchers.

const (
	// groNBuckets is the number of GRO buckets.
	groNBuckets = 8

	groNBucketsMask = groNBuckets - 1

	// groBucketSize is the size of each GRO bucket.
	groBucketSize = 8

	// groMaxPacketSize is the maximum size of a GRO'd packet.
	groMaxPacketSize = 1 << 16 // 65KB.
)

// A groBucket holds packets that are undergoing GRO.
type groBucket struct {
	// count is the number of packets in the bucket.
	count int

	// packets is the linked list of packets.
	packets groPacketList

	// packetsPrealloc and allocIdxs are used to preallocate and reuse
	// groPacket structs and avoid allocation.
	packetsPrealloc [groBucketSize]groPacket

	allocIdxs [groBucketSize]int
}

func (gb *groBucket) full() bool {
	return gb.count == groBucketSize
}

// insert inserts pkt into the bucket.
func (gb *groBucket) insert(pkt PacketBufferPtr, ipHdr header.IPv4, tcpHdr header.TCP, ep NetworkEndpoint) {
	groPkt := &gb.packetsPrealloc[gb.allocIdxs[gb.count]]
	*groPkt = groPacket{
		pkt:     pkt,
		created: time.Now(),
		ep:      ep,
		ipHdr:   ipHdr,
		tcpHdr:  tcpHdr,
	}
	gb.count++
	gb.packets.PushBack(groPkt)
}

// removeOldest removes the oldest packet from gb and returns the contained
// PacketBufferPtr. gb must not be empty.
func (gb *groBucket) removeOldest() PacketBufferPtr {
	pkt := gb.packets.Front()
	gb.packets.Remove(pkt)
	gb.count--
	gb.allocIdxs[gb.count] = pkt.idx
	ret := pkt.pkt
	*pkt = groPacket{}
	return ret
}

// removeOne removes a packet from gb. It also resets pkt to its zero value.
func (gb *groBucket) removeOne(pkt *groPacket) {
	gb.packets.Remove(pkt)
	gb.count--
	gb.allocIdxs[gb.count] = pkt.idx
	*pkt = groPacket{}
}

// A groPacket is packet undergoing GRO. It may be several packets coalesced
// together.
type groPacket struct {
	// groPacketEntry is an intrusive list.
	groPacketEntry

	// pkt is the coalesced packet.
	pkt PacketBufferPtr

	// ipHdr is the IP header for the coalesced packet.
	ipHdr header.IPv4

	// tcpHdr is the TCP header for the coalesced packet.
	tcpHdr header.TCP

	// created is when the packet was received.
	created time.Time

	// ep is the endpoint to which the packet will be sent after GRO.
	ep NetworkEndpoint

	// idx is the groPacket's index in its bucket packetsPrealloc. It is
	// immutable.
	idx int
}

// payloadSize is the payload size of the coalesced packet, which does not
// include the network or transport headers.
func (pk *groPacket) payloadSize() uint16 {
	return pk.ipHdr.TotalLength() - header.IPv4MinimumSize - uint16(pk.tcpHdr.DataOffset())
}

// groDispatcher coalesces incoming packets to increase throughput.
type groDispatcher struct {
	// newInterval notifies about changes to the interval.
	newInterval chan struct{}
	// intervalNS is the interval in nanoseconds.
	intervalNS atomicbitops.Int64
	// stop instructs the GRO dispatcher goroutine to stop.
	stop chan struct{}

	// mu protects the buckets.
	// TODO(b/256037250): This should be per-bucket.
	mu sync.Mutex
	// +checklocks:mu
	buckets [groNBuckets]groBucket
}

func (gd *groDispatcher) init(interval time.Duration) {
	gd.mu.Lock()
	defer gd.mu.Unlock()

	gd.intervalNS.Store(interval.Nanoseconds())
	gd.newInterval = make(chan struct{}, 1)
	gd.stop = make(chan struct{})

	for i := range gd.buckets {
		for j := range gd.buckets[i].packetsPrealloc {
			gd.buckets[i].allocIdxs[j] = j
			gd.buckets[i].packetsPrealloc[j].idx = j
		}
	}

	gd.start(interval)
}

// start spawns a goroutine that flushes the GRO periodically based on the
// interval.
func (gd *groDispatcher) start(interval time.Duration) {
	go func(interval time.Duration) {
		var ch <-chan time.Time
		if interval == 0 {
			// Never run.
			ch = make(<-chan time.Time)
		} else {
			ticker := time.NewTicker(interval)
			ch = ticker.C
		}
		for {
			select {
			case <-gd.newInterval:
				interval = time.Duration(gd.intervalNS.Load()) * time.Nanosecond
				if interval == 0 {
					// Never run. Flush any existing GRO packets.
					gd.flushAll()
					ch = make(<-chan time.Time)
				} else {
					ticker := time.NewTicker(interval)
					ch = ticker.C
				}
			case <-ch:
				gd.flush()
			case <-gd.stop:
				return
			}
		}
	}(interval)
}

func (gd *groDispatcher) getInterval() time.Duration {
	return time.Duration(gd.intervalNS.Load()) * time.Nanosecond
}

func (gd *groDispatcher) setInterval(interval time.Duration) {
	gd.intervalNS.Store(interval.Nanoseconds())
	gd.newInterval <- struct{}{}
}

// dispatch sends pkt up the stack after it undergoes GRO coalescing.
func (gd *groDispatcher) dispatch(pkt PacketBufferPtr, netProto tcpip.NetworkProtocolNumber, ep NetworkEndpoint, mtu uint32) {
	// If GRO is disabled simply pass the packet along.
	if gd.intervalNS.Load() == 0 {
		ep.HandlePacket(pkt)
		return
	}

	// Immediately get the IPv4 and TCP headers. We need a way to hash the
	// packet into its bucket, which requires addresses and ports. Linux
	// simply gets a hash passed by hardware, but we're not so lucky.

	// We only GRO IPv4 packets.
	if netProto != header.IPv4ProtocolNumber {
		ep.HandlePacket(pkt)
		return
	}

	// We only GRO TCP4 packets. The check for the transport protocol
	// number is done below so that we can PullUp both the IP and TCP
	// headers together.
	hdrBytes, ok := pkt.Data().PullUp(header.IPv4MinimumSize + header.TCPMinimumSize)
	if !ok {
		ep.HandlePacket(pkt)
		return
	}
	ipHdr := header.IPv4(hdrBytes)

	// We only handle atomic packets. That's the vast majority of traffic,
	// and simplifies handling.
	if ipHdr.FragmentOffset() != 0 || ipHdr.Flags()&header.IPv4FlagMoreFragments != 0 || ipHdr.Flags()&header.IPv4FlagDontFragment == 0 {
		ep.HandlePacket(pkt)
		return
	}

	// We only handle TCP packets without IP options.
	if ipHdr.HeaderLength() != header.IPv4MinimumSize || tcpip.TransportProtocolNumber(ipHdr.Protocol()) != header.TCPProtocolNumber {
		ep.HandlePacket(pkt)
		return
	}
	tcpHdr := header.TCP(hdrBytes[header.IPv4MinimumSize:])
	dataOff := tcpHdr.DataOffset()
	if dataOff < header.TCPMinimumSize {
		// Malformed packet: will be handled further up the stack.
		ep.HandlePacket(pkt)
		return
	}
	hdrBytes, ok = pkt.Data().PullUp(header.IPv4MinimumSize + int(dataOff))
	if !ok {
		// Malformed packet: will be handled further up the stack.
		ep.HandlePacket(pkt)
		return
	}

	tcpHdr = header.TCP(hdrBytes[header.IPv4MinimumSize:])

	// If either checksum is bad, flush the packet. Since we don't know
	// what bits were flipped, we can't identify this packet with a flow.
	tcpPayloadSize := ipHdr.TotalLength() - header.IPv4MinimumSize - uint16(dataOff)
	if !pkt.RXChecksumValidated {
		if !ipHdr.IsValid(pkt.Data().Size()) || !ipHdr.IsChecksumValid() {
			ep.HandlePacket(pkt)
			return
		}
		payloadChecksum := pkt.Data().ChecksumAtOffset(header.IPv4MinimumSize + int(dataOff))
		if !tcpHdr.IsChecksumValid(ipHdr.SourceAddress(), ipHdr.DestinationAddress(), payloadChecksum, tcpPayloadSize) {
			ep.HandlePacket(pkt)
			return
		}
		// We've validated the checksum, no reason for others to do it
		// again.
		pkt.RXChecksumValidated = true
	}

	// Now we can get the bucket for the packet.
	gd.mu.Lock()
	defer gd.mu.Unlock()

	bucket := &gd.buckets[gd.bucketForPacket(ipHdr, tcpHdr)&groNBucketsMask]
	groPkt, flushGROPkt := findGROPacket(bucket, ipHdr, tcpHdr)

	// Flush groPkt or merge the packets.
	flags := tcpHdr.Flags()
	if flushGROPkt {
		// Flush the existing GRO packet.
		ep.HandlePacket(groPkt.pkt)
		bucket.removeOne(groPkt)
		groPkt = nil
	} else if groPkt != nil {
		// Merge pkt in to GRO packet.
		buf := pkt.Data().ToBuffer()
		buf.TrimFront(header.IPv4MinimumSize + int64(dataOff))
		groPkt.pkt.Data().MergeBuffer(&buf)
		buf.Release()
		// Add flags from the packet to the GRO packet.
		groPkt.tcpHdr.SetFlags(uint8(groPkt.tcpHdr.Flags() | (flags & (header.TCPFlagFin | header.TCPFlagPsh))))
		// Update the IP total length.
		groPkt.ipHdr.SetTotalLength(groPkt.ipHdr.TotalLength() + uint16(tcpPayloadSize))

		pkt = PacketBufferPtr{}
	}

	// Flush if the packet isn't MSS-sized or if certain flags are set. The
	// reason for checking MSS equality is:
	// - If the packet is smaller than the MSS, this is likely the end of
	//   some message. Peers will send MSS-sized packets until they have
	//   insufficient data to do so.
	// - If the packet is larger than MSS, this packet is either malformed,
	//   a local GSO packet, or has already been handled by host GRO.
	// TODO(b/256037250): Use MSS instead of MTU.
	flush := uint32(ipHdr.TotalLength()) != mtu || header.TCPFlags(flags)&(header.TCPFlagUrg|header.TCPFlagPsh|header.TCPFlagRst|header.TCPFlagSyn|header.TCPFlagFin) != 0

	switch {
	case flush && groPkt != nil:
		// A merge occurred and we need to flush groPkt.
		ep.HandlePacket(groPkt.pkt)
		bucket.removeOne(groPkt)
	case flush && groPkt == nil:
		// No merge occurred and the incoming packet needs to be flushed.
		ep.HandlePacket(pkt)
	case !flush && groPkt == nil:
		// New flow and we don't need to flush. Insert pkt into GRO.
		if bucket.full() {
			// Head is always the oldest packet
			ep.HandlePacket(bucket.removeOldest())
		}
		bucket.insert(pkt.IncRef(), ipHdr, tcpHdr, ep)
	}
}

// findGROPacket returns the groPkt that matches ipHdr and tcpHdr, or nil if
// none exists. It also returns whether the groPkt should be flushed based on
// differences between the two headers.
func findGROPacket(bucket *groBucket, ipHdr header.IPv4, tcpHdr header.TCP) (*groPacket, bool) {
	for groPkt := bucket.packets.Front(); groPkt != nil; groPkt = groPkt.Next() {
		// Do the addresses match?
		if ipHdr.SourceAddress() != groPkt.ipHdr.SourceAddress() || ipHdr.DestinationAddress() != groPkt.ipHdr.DestinationAddress() {
			continue
		}

		// Do the ports match?
		if tcpHdr.SourcePort() != groPkt.tcpHdr.SourcePort() || tcpHdr.DestinationPort() != groPkt.tcpHdr.DestinationPort() {
			continue
		}

		// We've found a packet of the same flow.

		// IP checks.
		TOS, _ := ipHdr.TOS()
		groTOS, _ := groPkt.ipHdr.TOS()
		if ipHdr.TTL() != groPkt.ipHdr.TTL() || TOS != groTOS {
			return groPkt, true
		}

		// TCP checks.
		flags := tcpHdr.Flags()
		groPktFlags := groPkt.tcpHdr.Flags()
		dataOff := tcpHdr.DataOffset()
		if flags&header.TCPFlagCwr != 0 || // Is congestion control occurring?
			(flags^groPktFlags)&^(header.TCPFlagCwr|header.TCPFlagFin|header.TCPFlagPsh) != 0 || // Do the flags differ besides CRW, FIN, and PSH?
			tcpHdr.AckNumber() != groPkt.tcpHdr.AckNumber() || // Do the ACKs match?
			dataOff != groPkt.tcpHdr.DataOffset() || // Are the TCP headers the same length?
			groPkt.tcpHdr.SequenceNumber()+uint32(groPkt.payloadSize()) != tcpHdr.SequenceNumber() { // Does the incoming packet match the expected sequence number?
			return groPkt, true
		}
		// The options, including timestamps, must be identical.
		for i := header.TCPMinimumSize; i < int(dataOff); i++ {
			if tcpHdr[i] != groPkt.tcpHdr[i] {
				return groPkt, true
			}
		}

		// There's an upper limit on coalesced packet size.
		if int(ipHdr.TotalLength())-header.IPv4MinimumSize-int(dataOff)+groPkt.pkt.Data().Size() >= groMaxPacketSize {
			return groPkt, true
		}

		return groPkt, false
	}

	return nil, false
}

func (gd *groDispatcher) bucketForPacket(ipHdr header.IPv4, tcpHdr header.TCP) int {
	// TODO(b/256037250): Use jenkins or checksum. Write a test to print
	// distribution.
	var sum int
	for _, val := range []byte(ipHdr.SourceAddress()) {
		sum += int(val)
	}
	for _, val := range []byte(ipHdr.DestinationAddress()) {
		sum += int(val)
	}
	sum += int(tcpHdr.SourcePort())
	sum += int(tcpHdr.DestinationPort())
	return sum
}

// flush sends any packets older than interval up the stack.
func (gd *groDispatcher) flush() {
	interval := gd.intervalNS.Load()
	oldTime := time.Now().Add(-time.Duration(interval) * time.Nanosecond)

	gd.mu.Lock()
	defer gd.mu.Unlock()

	for i := range gd.buckets {
		bucket := &gd.buckets[i]
		for groPkt := bucket.packets.Front(); groPkt != nil; groPkt = groPkt.Next() {
			if groPkt.created.Before(oldTime) {
				groPkt.ep.HandlePacket(groPkt.pkt)
				bucket.removeOne(groPkt)
			} else {
				// Packets are ordered by age, so we can move
				// on once we find one that's too new.
				break
			}
		}
	}
}

func (gd *groDispatcher) flushAll() {
	gd.mu.Lock()
	defer gd.mu.Unlock()

	for i := range gd.buckets {
		bucket := &gd.buckets[i]
		for groPkt := bucket.packets.Front(); groPkt != nil; groPkt = groPkt.Next() {
			groPkt.ep.HandlePacket(groPkt.pkt)
			bucket.removeOne(groPkt)
		}
	}

}

// close stops the GRO goroutine and releases any held packets.
func (gd *groDispatcher) close() {
	gd.stop <- struct{}{}

	gd.mu.Lock()
	defer gd.mu.Unlock()

	for i := range gd.buckets {
		bucket := &gd.buckets[i]
		for groPkt := bucket.packets.Front(); groPkt != nil; groPkt = groPkt.Next() {
			groPkt.pkt.DecRef()
		}
	}
}

// String implements fmt.Stringer.
func (gd *groDispatcher) String() string {
	gd.mu.Lock()
	defer gd.mu.Unlock()

	ret := "GRO state: \n"
	for i, bucket := range gd.buckets {
		ret += fmt.Sprintf("bucket %d: %d packets: ", i, bucket.count)
		for groPkt := bucket.packets.Front(); groPkt != nil; groPkt = groPkt.Next() {
			ret += fmt.Sprintf("%s (%d), ", groPkt.created, groPkt.pkt.Data().Size())
		}
		ret += "\n"
	}
	return ret
}