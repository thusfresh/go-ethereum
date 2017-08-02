// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package network

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/pot"
)

/*

Taking the proximity order relative to a fix point x classifies the points in
the space (n byte long byte sequences) into bins. Items in each are at
most half as distant from x as items in the previous bin. Given a sample of
uniformly distributed items (a hash function over arbitrary sequence) the
proximity scale maps onto series of subsets with cardinalities on a negative
exponential scale.

It also has the property that any two item belonging to the same bin are at
most half as distant from each other as they are from x.

If we think of random sample of items in the bins as connections in a network of
interconnected nodes than relative proximity can serve as the basis for local
decisions for graph traversal where the task is to find a route between two
points. Since in every hop, the finite distance halves, there is
a guaranteed constant maximum limit on the number of hops needed to reach one
node from the other.
*/

var pof = pot.DefaultPof(256)

// KadParams holds the config params for Kademlia
type KadParams struct {
	// adjustable parameters
	MaxProxDisplay int  // number of rows the table shows
	MinProxBinSize int  // nearest neighbour core minimum cardinality
	MinBinSize     int  // minimum number of peers in a row
	MaxBinSize     int  // maximum number of peers in a row before pruning
	RetryInterval  int  // initial interval before a peer is first redialed
	RetryExponent  int  // exponent to multiply retry intervals with
	MaxRetries     int  // maximum number of redial attempts
	PruneInterval  int  // interval between peer pruning cycles
	HealthCheck    bool // whether kademlia On should signal completion
}

// NewKadParams returns a params struct with default values
func NewKadParams() *KadParams {
	return &KadParams{
		MaxProxDisplay: 8,
		MinProxBinSize: 2,
		MinBinSize:     2,
		MaxBinSize:     4,
		//RetryInterval:  42000000000,
		RetryInterval: 420000000,
		MaxRetries:    42,
		RetryExponent: 2,
		PruneInterval: 0, // TODO:
		HealthCheck:   false,
	}
}

// Kademlia is a table of live peers and a db of known peers
type Kademlia struct {
	lock         sync.RWMutex
	*KadParams            // Kademlia configuration parameters
	base         []byte   // immutable baseaddress of the table
	addrs        *pot.Pot // pots container for known peer addresses
	conns        *pot.Pot // pots container for live peer connections
	currentDepth uint8    // stores the last calculated depth
	events       chan struct{}
}

// NewKademlia creates a Kademlia table for base address addr
// with parameters as in params
// if params is nil, it uses default values
func NewKademlia(addr []byte, params *KadParams) *Kademlia {
	if params == nil {
		params = NewKadParams()
	}
	var events chan struct{}
	if params.HealthCheck {
		events = make(chan struct{}, 1)
	}
	return &Kademlia{
		base:      addr,
		KadParams: params,
		addrs:     pot.NewPot(nil, 0),
		conns:     pot.NewPot(nil, 0),
		events:    events,
	}
}

// Notifier interface type for peer allowing / requesting peer and depth notifications
type Notifier interface {
	NotifyPeer(OverlayAddr, uint8) error
	NotifyDepth(uint8) error
}

// OverlayPeer interface captures the common aspect of view of a peer from the Overlay
// topology driver
type OverlayPeer interface {
	Address() []byte
}

// OverlayConn represents a connected peer
type OverlayConn interface {
	OverlayPeer
	Drop(error)       // call to indicate a peer should be expunged
	Off() OverlayAddr // call to return a persitent OverlayAddr
}

// OverlayAddr represents a kademlia peer record
type OverlayAddr interface {
	OverlayPeer
	Update(OverlayAddr) OverlayAddr // returns the updated version of the original
}

// entry represents a Kademlia table entry (an extension of OverlayPeer)
type entry struct {
	OverlayPeer
	seenAt  time.Time
	retries int
}

// newEntry creates a kademlia peer from an OverlayPeer interface
func newEntry(p OverlayPeer) *entry {
	return &entry{
		OverlayPeer: p,
		seenAt:      time.Now(),
	}
}

// Bin is the binary (bitvector) serialisation of the entry address
func (e *entry) Bin() string {
	return pot.ToBin(e.addr().Address())
}

// Label is a short tag for the entry for debug
func Label(e *entry) string {
	return fmt.Sprintf("%s (%d)", e.Bin()[:8], e.retries)
}

// Hex is the hexadecimal serialisation of the entry address
func (e *entry) Hex() string {
	return fmt.Sprintf("%x", e.addr().Address())
}

// String is the short tag for the entry
func (e *entry) String() string {
	return fmt.Sprintf("%s (%d)", e.Hex()[:4], e.retries)
}

// addr returns the kad peer record (OverlayAddr) corresponding to the entry
func (e *entry) addr() OverlayAddr {
	a, _ := e.OverlayPeer.(OverlayAddr)
	return a
}

// conn returns the connected peer (OverlayPeer) corresponding to the entry
func (e *entry) conn() OverlayConn {
	c, _ := e.OverlayPeer.(OverlayConn)
	return c
}

// Register enters each OverlayAddr as kademlia peer record into the
// database of known peer addresses
func (k *Kademlia) Register(peers chan OverlayAddr) error {
	np := pot.NewPot(nil, 0)
	for p := range peers {
		// error if self received, peer should know better
		if bytes.Equal(p.Address(), k.base) {
			return fmt.Errorf("add peers: %x is self", k.base)
		}
		np, _, _ = pot.Add(np, newEntry(p), pof)
	}
	var com int
	log.Trace(fmt.Sprintf("%x registering peers", k.BaseAddr()[:4]))
	k.lockLock("Register")
	log.Trace(fmt.Sprintf("%x got lock", k.BaseAddr()[:4]))
	defer k.lockUnlock("Register")
	k.addrs, com = pot.Union(k.addrs, np, pof)
	log.Trace(fmt.Sprintf("%x merged %v peers, %v known, total: %v", k.BaseAddr()[:4], np.Size(), com, k.addrs.Size()))
	return nil
}

func (k *Kademlia) lockLock(s string) {
	log.Trace(fmt.Sprintf("%x locking %s", k.BaseAddr()[:4], s))
	k.lock.Lock()
	log.Trace(fmt.Sprintf("%x locked %s", k.BaseAddr()[:4], s))
}
func (k *Kademlia) lockRLock(s string) {
	log.Trace(fmt.Sprintf("%x rlocking %s", k.BaseAddr()[:4], s))
	k.lock.RLock()
	log.Trace(fmt.Sprintf("%x rlocked %s", k.BaseAddr()[:4], s))
}
func (k *Kademlia) lockUnlock(s string) {
	log.Trace(fmt.Sprintf("%x unlocking %s", k.BaseAddr()[:4], s))
	k.lock.Unlock()
	log.Trace(fmt.Sprintf("%x unlocked %s", k.BaseAddr()[:4], s))
}
func (k *Kademlia) lockRUnlock(s string) {
	log.Trace(fmt.Sprintf("%x runlocking %s", k.BaseAddr()[:4], s))
	k.lock.RUnlock()
	log.Trace(fmt.Sprintf("%x runlocked %s", k.BaseAddr()[:4], s))
}

// SuggestPeer returns a known peer for the lowest proximity bin for the
// lowest bincount below depth
// naturally if there is an empty row it returns a peer for that
func (k *Kademlia) SuggestPeer() (a OverlayAddr, o int, want bool) {
	log.Trace(fmt.Sprintf("%x registering peers", k.BaseAddr()[:4]))
	k.lockRLock("Sugg")
	defer k.lockRUnlock("Sugg")
	minsize := k.MinBinSize
	depth := k.depth()
	// if there is a callable neighbour within the current proxBin, connect
	// this makes sure nearest neighbour set is fully connected
	var ppo int
	k.addrs.EachNeighbour(k.base, pof, func(val pot.Val, po int) bool {
		if po < depth {
			return false
		}
		a = k.callable(val)
		ppo = po
		return a == nil
	})
	if a != nil {
		log.Trace(fmt.Sprintf("%08x candidate nearest neighbour found: %v (%v)", k.BaseAddr()[:4], a, ppo))
		return a, 0, false
	}
	log.Trace(fmt.Sprintf("%08x no candidate nearest neighbours to connect to (Depth: %v, minProxSize: %v) %#v", k.BaseAddr()[:4], depth, k.MinProxBinSize, a))

	var bpo []int
	prev := -1
	k.conns.EachBin(k.base, pof, 0, func(po, size int, f func(func(val pot.Val, i int) bool) bool) bool {
		prev++
		for ; prev < po; prev++ {
			bpo = append(bpo, prev)
			minsize = 0
		}
		if size < minsize {
			bpo = append(bpo, po)
			minsize = size
		}
		return size > 0 && po < depth
	})
	// all buckets are full, ie., minsize == k.MinBinSize
	if len(bpo) == 0 {
		log.Debug(fmt.Sprintf("%08x: all bins saturated", k.BaseAddr()[:4]))
		return nil, 0, false
	}
	// as long as we got candidate peers to connect to
	// dont ask for new peers (want = false)
	// try to select a candidate peer
	// find the first callable peer
	nxt := bpo[0]
	k.addrs.EachBin(k.base, pof, nxt, func(po, _ int, f func(func(pot.Val, int) bool) bool) bool {
		// for each bin (up until depth) we find callable candidate peers
		if po >= depth {
			return false
		}
		f(func(val pot.Val, _ int) bool {
			a = k.callable(val)
			return a == nil
		})
		return false
	})
	// found a candidate
	if a != nil {
		return a, 0, false
	}
	// no candidate peer found, request for the short bin
	return a, nxt, true
}

// On inserts the peer as a kademlia peer into the live peers
func (k *Kademlia) On(p OverlayConn) {
	k.lockLock("On")
	defer k.lockUnlock("On")
	e := newEntry(p)
	var ins bool
	k.conns, _, _, _ = pot.Swap(k.conns, p, pof, func(v pot.Val) pot.Val {
		// if not found live
		if v == nil {
			ins = true
			// insert new online peer into conns
			return e
		}
		// found among live peers, do nothing
		return v
	})
	if ins {
		// insert new online peer into addrs
		k.addrs, _, _, _ = pot.Swap(k.addrs, p, pof, func(v pot.Val) pot.Val {
			return e
		})
	}
	if k.HealthCheck {
		k.events <- struct{}{}
	}
	go k.notify(p)
}

func (k *Kademlia) notify(p OverlayConn) {
	k.lockRLock("notify")
	defer k.lockRUnlock("notify")
	np, ok := p.(Notifier)
	if !ok {
		return
	}
	depth := uint8(k.depth())
	var depthChanged bool
	if depth != k.currentDepth {
		depthChanged = true
		k.currentDepth = depth
	}

	np.NotifyDepth(depth)
	f := func(val pot.Val, po int) {
		dp := val.(*entry).OverlayPeer.(Notifier)
		dp.NotifyPeer(p.Off(), uint8(po))
		log.Trace(fmt.Sprintf("peer %v notified of %v (%v)", dp, p, po))
		if depthChanged {
			dp.NotifyDepth(depth)
			log.Trace(fmt.Sprintf("peer %v notified of new depth %v", dp, depth))
		}
	}

	k.conns.EachNeighbourAsync(p, pof, 1024, 255, f, false)
}

// Off removes a peer from among live peers
func (k *Kademlia) Off(p OverlayConn) {
	k.lockLock("Off")
	defer k.lockUnlock("Off")
	var del bool
	k.addrs, _, _, _ = pot.Swap(k.addrs, p, pof, func(v pot.Val) pot.Val {
		// v cannot be nil, must check otherwise we overwrite entry
		if v == nil {
			panic(fmt.Sprintf("connected peer not found %v", p))
		}
		del = true
		return newEntry(p.Off())
	})
	if del {
		k.conns, _, _, _ = pot.Swap(k.conns, p, pof, func(_ pot.Val) pot.Val {
			// v cannot be nil, but no need to check
			return nil
		})
	}
}

// EachConn is an iterator with args (base, po, f) applies f to each live peer
// that has proximity order po or less as measured from the base
// if base is nil, kademlia base address is used
func (k *Kademlia) EachConn(base []byte, o int, f func(OverlayConn, int, bool) bool) {
	k.lockRLock("eachconn")
	defer k.lockRUnlock("eachconn")
	k.eachConn(base, o, f)
}

func (k *Kademlia) eachConn(base []byte, o int, f func(OverlayConn, int, bool) bool) {
	if len(base) == 0 {
		base = k.base
	}
	depth := k.depth()
	k.conns.EachNeighbour(base, pof, func(val pot.Val, po int) bool {
		if po > o {
			return true
		}
		return f(val.(*entry).conn(), po, po >= depth)
	})
}

// EachAddr called with (base, po, f) is an iterator applying f to each known peer
// that has proximity order po or less as measured from the base
// if base is nil, kademlia base address is used
func (k *Kademlia) EachAddr(base []byte, o int, f func(OverlayAddr, int) bool) {
	if len(base) == 0 {
		base = k.base
	}
	k.lockRLock("eachaddr")
	defer k.lockRUnlock("eachaddr")
	k.addrs.EachNeighbour(base, pof, func(val pot.Val, po int) bool {
		if po > o {
			return true
		}
		return f(val.(*entry).addr(), po)
	})
}

// Depth returns the proximity order that defines the distance of
// the nearest neighbour set with cardinality >= MinProxBinSize
// if there is altogether less than MinProxBinSize peers it returns 0
func (k *Kademlia) Depth() (depth int) {
	k.lockRLock("depth")
	defer k.lockRUnlock("depth")
	return k.depth()
}

func (k *Kademlia) depth() (depth int) {
	if k.conns.Size() < k.MinProxBinSize {
		return 0
	}
	var size int
	f := func(v pot.Val, i int) bool {
		size++
		depth = i
		return size < k.MinProxBinSize
	}
	k.conns.EachNeighbour(k.base, pof, f)
	return depth
}

// calleble when called with val,
func (k *Kademlia) callable(val pot.Val) OverlayAddr {
	e := val.(*entry)
	// not callable if peer is live or exceeded maxRetries
	// log.Trace(fmt.Sprintf("%08x peer %#v (%T)", k.BaseAddr()[:4], e.OverlayPeer, e.OverlayPeer))
	if e.conn() != nil || e.retries > k.MaxRetries {
		log.Trace(fmt.Sprintf("peer %v (%T) not callable", e, e.OverlayPeer))
		return nil
	}
	// calculate the allowed number of retries based on time lapsed since last seen
	timeAgo := time.Since(e.seenAt)
	var retries int
	for delta := int(timeAgo) / k.RetryInterval; delta > 0; delta /= k.RetryExponent {
		retries++
	}

	// this is never called concurrently, so safe to increment
	// peer can be retried again
	if retries < e.retries {
		log.Trace(fmt.Sprintf("%v long time since last try (at %v) needed before retry %v, wait only warrants %v", e, timeAgo, e.retries, retries))
		return nil
	}
	e.retries++
	log.Trace(fmt.Sprintf("peer %v is callable", e))

	return e.addr()
}

// BaseAddr return the kademlia base addres
func (k *Kademlia) BaseAddr() []byte {
	return k.base
}

// String returns kademlia table + kaddb table displayed with ascii
func (k *Kademlia) String() string {
	k.lockRLock("hive")
	defer k.lockRUnlock("hive")
	wsrow := "                          "
	var rows []string

	rows = append(rows, "=========================================================================")
	rows = append(rows, fmt.Sprintf("%v KΛÐΞMLIΛ hive: queen's address: %x", time.Now().UTC().Format(time.UnixDate), k.BaseAddr()[:3]))
	rows = append(rows, fmt.Sprintf("population: %d (%d), MinProxBinSize: %d, MinBinSize: %d, MaxBinSize: %d", k.conns.Size(), k.addrs.Size(), k.MinProxBinSize, k.MinBinSize, k.MaxBinSize))

	liverows := make([]string, k.MaxProxDisplay)
	peersrows := make([]string, k.MaxProxDisplay)

	depth := k.depth()
	rest := k.conns.Size()
	k.conns.EachBin(k.base, pof, 0, func(po, size int, f func(func(val pot.Val, i int) bool) bool) bool {
		var rowlen int
		if po >= k.MaxProxDisplay {
			po = k.MaxProxDisplay - 1
		}
		row := []string{fmt.Sprintf("%2d", size)}
		rest -= size
		f(func(val pot.Val, vpo int) bool {
			e := val.(*entry)
			row = append(row, fmt.Sprintf("%x", e.Address()[:2]))
			rowlen++
			return rowlen < 4
		})
		r := strings.Join(row, " ")
		r = r + wsrow
		liverows[po] = r[:31]
		return true
	})

	k.addrs.EachBin(k.base, pof, 0, func(po, size int, f func(func(val pot.Val, i int) bool) bool) bool {
		var rowlen int
		if po >= k.MaxProxDisplay {
			po = k.MaxProxDisplay - 1
		}
		if size < 0 {
			panic("wtf")
		}
		row := []string{fmt.Sprintf("%2d", size)}
		// we are displaying live peers too
		f(func(val pot.Val, vpo int) bool {
			row = append(row, val.(*entry).String())
			rowlen++
			return rowlen < 4
		})
		peersrows[po] = strings.Join(row, " ")
		return true
	})

	for i := 0; i < k.MaxProxDisplay; i++ {
		if i == depth {
			rows = append(rows, fmt.Sprintf("============ DEPTH: %d ==========================================", i))
		}
		left := liverows[i]
		right := peersrows[i]
		if len(left) == 0 {
			left = " 0                             "
		}
		if len(right) == 0 {
			right = " 0"
		}
		rows = append(rows, fmt.Sprintf("%03d %v | %v", i, left, right))
	}
	rows = append(rows, "=========================================================================")
	return "\n" + strings.Join(rows, "\n")
}

// Prune implements a forever loop reacting to a ticker time channel given
// as the first argument
// the loop quits if the channel is closed
// it checks each kademlia bin and if the peer count is higher than
// the MaxBinSize parameter it drops the oldest n peers such that
// the bin is reduced to MinBinSize peers thus leaving slots to newly
// connecting peers
func (k *Kademlia) Prune(c <-chan time.Time) {
	go func() {
		for range c {
			k.lockRLock("prune")
			conns := k.conns
			k.lockRUnlock("prune")
			total := 0
			conns.EachBin(k.base, pof, 0, func(po, size int, f func(func(pot.Val, int) bool) bool) bool {
				extra := size - k.MinBinSize
				if size > k.MaxBinSize {
					n := 0
					f(func(v pot.Val, po int) bool {
						v.(*entry).conn().Drop(fmt.Errorf("bucket full"))
						n++
						return n < extra
					})
					total += extra
				}
				return true
			})
			log.Trace(fmt.Sprintf("pruned %v peers", total))
		}
	}()
}

// PeerPot keeps info about expected nearest neighbours and empty bins
// used for testing only
type PeerPot struct {
	NNSet     [][]byte
	EmptyBins []int
}

// NewPeerPot just creates a new pot record OverlayAddr
func NewPeerPot(kadMinProxSize int, ids []discover.NodeID, addrs [][]byte) map[discover.NodeID]*PeerPot {
	// create a table of all nodes for health check
	np := pot.NewPot(nil, 0)
	for _, addr := range addrs {
		np, _, _ = pot.Add(np, addr, pof)
	}
	ppmap := make(map[discover.NodeID]*PeerPot)

	for i, id := range ids {
		pl := 256
		prev := 256
		var emptyBins []int
		var nns [][]byte
		np.EachNeighbour(addrs[i], pof, func(val pot.Val, po int) bool {
			a := val.([]byte)
			if po == 256 {
				return true
			}
			if pl == 256 || pl == po {
				nns = append(nns, a)
			}
			if pl == 256 && len(nns) >= kadMinProxSize {
				pl = po
				prev = po
			}
			if prev < pl {
				for j := prev; j > po; j-- {
					emptyBins = append(emptyBins, j)
				}
			}
			prev = po - 1
			return true
		})
		for j := prev; j >= 0; j-- {
			emptyBins = append(emptyBins, j)
		}
		log.Trace(fmt.Sprintf("%x NNS: %s", addrs[i][:4], logNNS(nns)))
		ppmap[id] = &PeerPot{nns, emptyBins}
	}
	return ppmap
}

func (k *Kademlia) full(emptyBins []int) (full bool) {
	prev := 0
	e := len(emptyBins)
	k.conns.EachBin(k.base, pof, 0, func(po, _ int, _ func(func(val pot.Val, i int) bool) bool) bool {
		for i := prev; e > 0 && i < po; i++ {
			e--
			if emptyBins[e] != i {
				log.Trace(fmt.Sprintf("%08x po: %d, i: %d, e: %d, emptybins: %v", k.BaseAddr()[:4], po, i, e, logEmptyBins(emptyBins)))
				if emptyBins[e] < i {
					panic("incorrect peerpot")
				}
				return false
			}
		}
		prev = po + 1
		return true
	})
	return e == 0
}

func (k *Kademlia) gotNearestNeighbours(peers [][]byte) bool {
	pm := make(map[string]bool)

	k.eachConn(nil, 255, func(p OverlayConn, po int, nn bool) bool {
		if !nn {
			return false
		}
		pk := fmt.Sprintf("%x", p.Address())
		pm[pk] = true
		return true
	})
	log.Trace(fmt.Sprintf("%08x: NNSet: %d", k.BaseAddr()[:4], len(pm)))
	for _, p := range peers {
		pk := fmt.Sprintf("%x", p)
		if !pm[pk] {
			log.Trace(fmt.Sprintf("%08x: ExpNN: %s not found", k.BaseAddr()[:4], pk))
			return false
		}
	}
	return true
}

// Healthy reports the health state of the kademlia connectivity
func (k *Kademlia) Healthy(pp *PeerPot) bool {
	log.Trace(fmt.Sprintf("%08x: healthy?", k.BaseAddr()[:4]))
	if k.HealthCheck {
		<-k.events
	}
	k.lockRLock("health")
	defer k.lockRUnlock("health")
	gotnn := k.gotNearestNeighbours(pp.NNSet)
	full := k.full(pp.EmptyBins)
	if !gotnn || !full {
		log.Trace(fmt.Sprintf("%08x: NNSet: %s, EmptyBins: %s", k.BaseAddr()[:4], logNNS(pp.NNSet), logEmptyBins(pp.EmptyBins)))
	}
	log.Trace(fmt.Sprintf("%08x: healthy: %v && %v", k.BaseAddr()[:4], gotnn, full))
	return gotnn && full
}

func logNNS(nns [][]byte) string {
	var nnsa []string
	for _, nn := range nns {
		nnsa = append(nnsa, fmt.Sprintf("%08x", nn[:4]))
	}
	return strings.Join(nnsa, ", ")
}

func logEmptyBins(ebs []int) string {
	var ebss []string
	for _, eb := range ebs {
		ebss = append(ebss, fmt.Sprintf("%d", eb))
	}
	return strings.Join(ebss, ", ")
}