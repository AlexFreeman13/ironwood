package network

import (
	crand "crypto/rand"
	"encoding/binary"
	mrand "math/rand"
	"time"

	//"fmt"

	"github.com/Arceliar/phony"

	"github.com/Arceliar/ironwood/network/internal/merkletree"
	"github.com/Arceliar/ironwood/types"
)

/***********
 * router *
 ***********/

/*

TODO: Testing
  The merkle tree logic hasn't been very thoroughly tested yet.
    In particular, when responding to a request, we skip over parts of the tree with only 1 non-empty child node.
    That logic is a bit delicate... some fuzz testing would probably be nice.
  Also need to test how expired node infos are handled.
    We mark an info as expired, and ignore it in lookups etc., after it times out.
    We don't delete the info until after 2 timeout periods.
    The intent is that this gives us some time to let all nodes mark the info as timed out, so we stop sending it as part of the merkle tree updates.
    Otherwise, info could hit a live-lock scenario where nothing ever expired in a dynamic enough network.
    However, that leads to issues with nodes disconnecting/reconnecting, and retting their sequence number.
    To combat this, if we decline an update but see that our local info is expired (and not equivalent to the update we saw), then we send back an announce for it.
    The intent is for this announce to travel back to the corresponding node, so they can quickly update their seq (if needed).
    Sending back an announce is a delicate matter -- if nodes disagree on what announcement is "better", then they can infinitely spam eachother.
    This means we need to be very sure the CRDT conflict resolution logic is eventually consistent.
  The max network size logic could use some futher testing.

TODO: Mostly ignore the above
  In the middle of rewriting things to not keep a full view of the network.
  Need to:
    Store a merkle tree per peer
    Fill the tree with our nodes ancestry + the peer's ancestry
    Don't store info that isn't needed for either
      Slightly subtle, we receive things from the merkle trees in no particular order, so it may not be clear that it's ancestry info for some node until much later.
      So we need to be careful about when we *delete* info, though if we don't (and just let it time out) then this doesn't really break anything, it's just a waste / opens the door to OOM issues (if a node spams irrelevant info -- though at least we don't forward it if they do).
      If we *did* delete things immediately when they're not seen to be important, then we would at least learn the peer's info when we sync merkle trees. Then the peer's parent. Then the parent's parent, etc., so it would finish eventually, it's just wasteful. But that suggets we could set a short timeout for anything not important (e.g. 1 minute) or delete a random not-important thing if the store becomes too full.
    Sync merkle trees with peers... somehow.
      Sending a request and our own root whenever we change anything at all would, technically, be sufficient I think... just wasteful.

*/

type router struct {
	phony.Inbox
	core       *core
	pathfinder pathfinder                       // see pathfinder.go
	blooms     blooms                           // see bloomfilter.go
	peers      map[publicKey]map[*peer]struct{} // True if we're allowed to send a mirror to this peer (but have not done so already)
	ports      map[peerPort]publicKey           // used in tree lookups
	merks      map[publicKey]merkletree.Tree    // Merkle tree per peered node, to synchronize any relevant state with that peer
	infos      map[publicKey]routerInfo
	timers     map[publicKey]*time.Timer
	cache      map[publicKey][]peerPort // Cache path slice for each peer
	requests   map[publicKey]routerSigReq
	responses  map[publicKey]routerSigRes
	resSeqs    map[publicKey]uint64
	resSeqCtr  uint64
	refresh    bool
	doRoot1    bool
	doRoot2    bool
	fixTimer   *time.Timer
}

func (r *router) init(c *core) {
	r.core = c
	r.pathfinder.init(r)
	r.blooms.init(r)
	r.peers = make(map[publicKey]map[*peer]struct{})
	r.ports = make(map[peerPort]publicKey)
	r.merks = make(map[publicKey]merkletree.Tree)
	r.infos = make(map[publicKey]routerInfo)
	r.timers = make(map[publicKey]*time.Timer)
	r.cache = make(map[publicKey][]peerPort)
	r.requests = make(map[publicKey]routerSigReq)
	r.responses = make(map[publicKey]routerSigRes)
	r.resSeqs = make(map[publicKey]uint64)
	// Kick off actor to do initial work / become root
	r.fixTimer = time.AfterFunc(0, func() {})
	r.doRoot2 = true
	r.Act(nil, r._fix)
}

func (r *router) _shutdown() {} // TODO cleanup (stop any timers etc)

func (r *router) _resetCache() {
	for k := range r.cache {
		delete(r.cache, k)
	}
}

func (r *router) addPeer(from phony.Actor, p *peer) {
	r.Act(from, func() {
		//r._resetCache()
		if _, isIn := r.peers[p.key]; !isIn {
			r.peers[p.key] = make(map[*peer]struct{})
			r.ports[p.port] = p.key
			r.blooms._addInfo(p.key)
			r.merks[p.key] = merkletree.Tree{}
			defer r._fixMerks()
		} else {
			defer func() {
				res := routerMerkleRes{
					digest: r.merks[p.key].Root.Digest,
				}
				p.sendMerkleRes(r, &res)
				p.sendMerkleReq(r, new(routerMerkleReq))
			}()
		}
		r.peers[p.key][p] = struct{}{}
		if _, isIn := r.responses[p.key]; !isIn {
			if _, isIn := r.requests[p.key]; !isIn {
				r.requests[p.key] = *r._newReq()
			}
			req := r.requests[p.key]
			p.sendSigReq(r, &req)
		}
		r.blooms._sendBloom(p)
	})
}

func (r *router) removePeer(from phony.Actor, p *peer) {
	r.Act(from, func() {
		//r._resetCache()
		ps := r.peers[p.key]
		delete(ps, p)
		if len(ps) == 0 {
			delete(r.peers, p.key)
			delete(r.requests, p.key)
			delete(r.responses, p.key)
			delete(r.resSeqs, p.key)
			delete(r.ports, p.port)
			delete(r.cache, p.key)
			r.blooms._removeInfo(p.key)
			r._fix()
		}
	})
}

func (r *router) _clearReqs() {
	for k := range r.requests {
		delete(r.requests, k)
	}
	for k := range r.responses {
		delete(r.responses, k)
	}
	for k := range r.resSeqs {
		delete(r.resSeqs, k)
	}
	r.resSeqCtr = 0
}

func (r *router) _sendReqs() {
	r._clearReqs()
	for pk, ps := range r.peers {
		req := r._newReq()
		r.requests[pk] = *req
		for p := range ps {
			p.sendSigReq(r, req)
		}
	}
}

func (r *router) _fix() {
	defer r._fixMerks()
	defer r.blooms._sendAllBlooms(true) // FIXME this is bad, hack for not tracking when tree relationships change
	defer r.blooms._fixOnTree()         // Defer second, so it happens first
	bestRoot := r.core.crypto.publicKey
	bestParent := r.core.crypto.publicKey
	self := r.infos[r.core.crypto.publicKey]
	// Check if our current parent leads to a better root than ourself
	if _, isIn := r.peers[self.parent]; isIn {
		root, _ := r._getRootAndDists(r.core.crypto.publicKey)
		if root.less(bestRoot) {
			bestRoot, bestParent = root, self.parent
		}
	}
	// Check if we know a better root/parent
	for pk := range r.responses {
		if _, isIn := r.infos[pk]; !isIn {
			// We don't know where this peer is
			continue
		}
		pRoot, pDists := r._getRootAndDists(pk)
		if _, isIn := pDists[r.core.crypto.publicKey]; isIn {
			// This would loop through us already
			continue
		}
		if pRoot.less(bestRoot) {
			bestRoot, bestParent = pRoot, pk
		}
		// TODO? switch to another parent (to the same root) if they're "better", at least sometimes
		if r.refresh || bestParent != self.parent {
			if pRoot == bestRoot && r.resSeqs[pk] < r.resSeqs[bestParent] {
				bestRoot, bestParent = pRoot, pk
			}
		}
	}
	if r.refresh || r.doRoot1 || r.doRoot2 || self.parent != bestParent {
		res, isIn := r.responses[bestParent]
		switch {
		case isIn && bestRoot != r.core.crypto.publicKey: // && t._useResponse(bestParent, &res):
			// Somebody else should be root
			if !r._useResponse(bestParent, &res) {
				panic("this should never happen")
			}
			r.fixTimer.Stop()
			r.refresh = false
			r.doRoot1 = false
			r.doRoot2 = false // TODO panic to check that this was already false
			r._sendReqs()
		case r.doRoot2:
			// Become root
			if !r._becomeRoot() {
				panic("this should never happen")
			}
			/*
				self = r.infos[r.core.crypto.publicKey]
				ann := self.getAnnounce(r.core.crypto.publicKey)
				for _, ps := range r.peers {
					for p := range ps {
						p.sendAnnounce(r, ann)
					}
				}
			*/
			r.refresh = false
			r.doRoot1 = false
			r.doRoot2 = false
			r.fixTimer.Stop()
			r._sendReqs()
		case !r.doRoot1:
			r.fixTimer = time.AfterFunc(time.Second, func() {
				r.Act(nil, func() {
					if r.doRoot1 {
						r.doRoot2 = true
						r._fix()
					}
				})
			})
			r.doRoot1 = true
			// No need to sendReqs in this case
			//  either we already have a req, or we've already requested one
			//  so resetting and re-requesting is just a waste of bandwidth
		default:
			// We need to self-root, but we already started a timer to do that later
			// So this is a no-op
		}
	}
}

func (r *router) _fixMerks() {
	var merk merkletree.Tree
	if !bloomMulticastEnabled {
		for k, info := range r.infos {
			ann := info.getAnnounce(k)
			bs, err := ann.encode(nil)
			if err != nil {
				panic("this should never happen")
			}
			digest := merkletree.GetDigest(bs)
			merk.Add(merkletree.Key(k), digest)
		}
	}
	for k, orig := range r.merks {
		if !bloomMulticastEnabled {
			// Merk is already in the right state, don't touch it
		} else {
			merk = merkletree.Tree{}
			panic("TODO")
		}
		if merk.Root.Digest == orig.Root.Digest {
			continue
		}
		r.merks[k] = merk
		res := routerMerkleRes{
			digest: merk.Root.Digest,
		}
		var req routerMerkleReq
		for p := range r.peers[k] {
			p.sendMerkleRes(r, &res)
			p.sendMerkleReq(r, &req)
		}
	}
}

func (r *router) _newReq() *routerSigReq {
	var req routerSigReq
	nonce := make([]byte, 8)
	crand.Read(nonce) // If there's an error, there's not much to do...
	req.nonce = binary.BigEndian.Uint64(nonce)
	req.seq = r.infos[r.core.crypto.publicKey].seq + 1
	return &req
}

func (r *router) _becomeRoot() bool {
	req := r._newReq()
	res := routerSigRes{
		routerSigReq: *req,
		port:         0, // TODO? something sane?
	}
	res.psig = r.core.crypto.privateKey.sign(res.bytesForSig(r.core.crypto.publicKey, r.core.crypto.publicKey))
	ann := routerAnnounce{
		key:          r.core.crypto.publicKey,
		parent:       r.core.crypto.publicKey,
		routerSigRes: res,
		sig:          res.psig,
	}
	if !ann.check() {
		panic("this should never happen")
	}
	return r._update(&ann)
}

func (r *router) _handleRequest(p *peer, req *routerSigReq) {
	res := routerSigRes{
		routerSigReq: *req,
		port:         p.port,
	}
	res.psig = r.core.crypto.privateKey.sign(res.bytesForSig(p.key, r.core.crypto.publicKey))
	p.sendSigRes(r, &res)
}

func (r *router) handleRequest(from phony.Actor, p *peer, req *routerSigReq) {
	r.Act(from, func() {
		r._handleRequest(p, req)
	})
}

func (r *router) _handleResponse(p *peer, res *routerSigRes) {
	if _, isIn := r.responses[p.key]; !isIn && r.requests[p.key] == res.routerSigReq {
		r.resSeqCtr++
		r.resSeqs[p.key] = r.resSeqCtr
		r.responses[p.key] = *res
		r._fix() // This could become our new parent
	}
}

func (r *router) _useResponse(peerKey publicKey, res *routerSigRes) bool {
	bs := res.bytesForSig(r.core.crypto.publicKey, peerKey)
	info := routerInfo{
		parent:       peerKey,
		routerSigRes: *res,
		sig:          r.core.crypto.privateKey.sign(bs),
	}
	ann := info.getAnnounce(r.core.crypto.publicKey)
	if r._update(ann) {
		/*
			for _, ps := range r.peers {
				for p := range ps {
					p.sendAnnounce(r, ann)
				}
			}
		*/
		return true
	}
	return false
}

func (r *router) handleResponse(from phony.Actor, p *peer, res *routerSigRes) {
	r.Act(from, func() {
		r._handleResponse(p, res)
	})
}

func (r *router) _update(ann *routerAnnounce) bool {
	if info, isIn := r.infos[ann.key]; isIn {
		switch {
		// Note: This logic *must* be the same on every node
		// If that's not true, then peers can infinitely spam announcements at each other for expired infos
		/*********************************
		 * XXX *** DO NOT CHANGE *** XXX *
		 *********************************/
		case info.seq > ann.seq:
			// This is an old seq, so exit
			return false
		case info.seq < ann.seq:
			// This is a newer seq, so don't exit
		case info.parent.less(ann.parent):
			// same seq, worse (higher) parent
			return false
		case ann.parent.less(info.parent):
			// same seq, better (lower) parent, so don't exit
		case ann.nonce < info.nonce:
			// same seq and parent, lower nonce, so don't exit
		default:
			// same seq and parent, same or worse nonce, so exit
			return false
		}
	}
	r._resetCache()
	info := routerInfo{
		parent:       ann.parent,
		routerSigRes: ann.routerSigRes,
		sig:          ann.sig,
	}
	key := ann.key
	var timer *time.Timer
	if key == r.core.crypto.publicKey {
		delay := r.core.config.routerRefresh + time.Millisecond*time.Duration(mrand.Intn(1024))
		timer = time.AfterFunc(delay, func() {
			r.Act(nil, func() {
				if r.timers[key] == timer {
					r.refresh = true
					r._fix()
				}
			})
		})
	} else {
		timer = time.AfterFunc(r.core.config.routerTimeout, func() {
			r.Act(nil, func() {
				if r.timers[key] == timer {
					info, isIn := r.infos[key]
					if !isIn || info.expired {
						timer.Stop() // Shouldn't matter, but just to be safe...
						// TODO figure out if this is really the right thing to do
						/*
							for k, merk := range r.merks { // Shouldn't be needed, but just to be safe...
								merk.Remove(merkletree.Key(key))
								r.merks[k] = merk
							}
						*/
						//r.merk.Remove(merkletree.Key(key)) // Shouldn't be needed, but just to be safe...
						delete(r.infos, key)
						delete(r.timers, key)
						r._resetCache()
						r._fix()
					} else {
						info.expired = true
						r.infos[key] = info
						// TODO figure out if this is really the right thing to do
						/*
							for k, merk := range r.merks { // Shouldn't be needed, but just to be safe...
								merk.Remove(merkletree.Key(key))
								r.merks[k] = merk
							}
						*/
						//r.merk.Remove(merkletree.Key(key))
						timer.Reset(time.Duration(2) * r.core.config.routerTimeout)
						r._fix()
					}
				}
			})
		})
	}
	if oldTimer, isIn := r.timers[key]; isIn {
		oldTimer.Stop()
	}
	/*
		bs, _ := ann.encode(nil)
		digest := merkletree.GetDigest(bs)
		for k, merk := range r.merks { // TODO figure out if this is really the right thing to do
			merk.Add(merkletree.Key(key), digest)
			r.merks[k] = merk
		}
	*/
	//r.merk.Add(merkletree.Key(key), digest)
	r.infos[key] = info
	r.timers[key] = timer
	return true
}

func (r *router) _handleAnnounce(sender *peer, ann *routerAnnounce) {
	var doUpdate bool
	var worst publicKey
	var found bool
	if len(r.infos) < int(r.core.config.routerMaxInfos) {
		// We're not at max capacity yet, so we have room to add more
		doUpdate = true
	} else if _, isIn := r.infos[ann.key]; isIn {
		// We're at capacity (or, somehow, above) but we alread know about this
		// Therefore, there's no harm in accepting the update (we can't force anything else out)
		// If this was or last check, then this is basically TOFU for the network
		doUpdate = true
	} else {
		// We're at or above capacity, and this is a new node
		// It may be "better" than something we already know about
		// We define better to mean lower key (so e.g. we all know the root)
		// We also special case or own info, to avoid timer problems
		for k := range r.infos {
			if k == r.core.crypto.publicKey {
				// Skip self
				continue
			}
			if !found || worst.less(k) {
				// This is the worst (non-self) node we've seen so far
				worst = k
				found = true
			}
		}
		if ann.key.less(worst) {
			// This means ann.key is better than some node we already know
			// We will try to _update, and remove the worst node if we do
			doUpdate = true
		}
	}
	if !doUpdate {
		return
	}
	if r._update(ann) {
		/*
			for _, ps := range r.peers {
				for p := range ps {
					if p == sender {
						continue
					}
					p.sendAnnounce(r, ann)
				}
			}
		*/
		if found {
			// Cleanup worst
			r.timers[worst].Stop()
			/*
				for k, merk := range r.merks {
					merk.Remove(merkletree.Key(worst))
					r.merks[k] = merk
				}
			*/
			//r.merk.Remove(merkletree.Key(worst))
			delete(r.infos, worst)
			delete(r.timers, worst)
			r._resetCache()
		}
		if ann.key == r.core.crypto.publicKey {
			// We just updated our own info from a message we received by a peer
			// That suggests we went offline, so our seq reset when we came back
			// The info they sent us could have been expired (see below in this function)
			// So we need to set that an update is required, as if our refresh timer has passed
			r.refresh = true
		}
		r._fix() // This could require us to change parents
	} else {
		// We didn't accept the update
		// If our current info is expired, then tell the sender about it
		// That *should* find its way back to the original node, so they can update seqs etc more quickly...
		/* TODO?
		if info := r.infos[ann.key]; info.expired {
			newAnn := info.getAnnounce(ann.key)
			if *newAnn != *ann {
				sender.sendAnnounce(r, newAnn)
			}
		}
		*/
	}
}

func (r *router) handleAnnounce(from phony.Actor, p *peer, ann *routerAnnounce) {
	r.Act(from, func() {
		r._handleAnnounce(p, ann)
	})
}

func (r *router) handleMerkleReq(from phony.Actor, p *peer, req *routerMerkleReq) {
	r.Act(from, func() {
		merk := r.merks[p.key]
		node, plen := merk.NodeFor(merkletree.Key(req.prefix), int(req.prefixLen))
		if uint64(plen) != req.prefixLen {
			// We don't know anyone from the part of the network we were asked about, so we can't respond in any useful way
			return
		}
		/*
			// This is the "safe" but extra inefficient version of things
			res := new(routerMerkleRes)
			res.prefixLen = req.prefixLen
			res.prefix = req.prefix
			res.digest = node.Digest
			p.sendMerkleRes(r, res)
			if res.prefixLen == merkletree.KeyBits {
				if info, isIn := r.infos[res.prefix]; isIn {
					p.sendAnnounce(r, info.getAnnounce(res.prefix))
				} else {
					panic("this should never happen")
				}
			}
			return
		*/
		// This is the slightly less inefficient but very delicate version of things
		// Basically, if we get to a node that only has 1 child, follow it until we have 2 (or reach the end, to send a node announcement instead)
		// TODO we need to test this thoroughly
		prefixLen := req.prefixLen
		prefix := req.prefix
		for {
			if node.Left != nil && node.Right != nil {
				res := new(routerMerkleRes)
				res.prefixLen = prefixLen
				res.prefix = prefix
				res.digest = node.Digest
				p.sendMerkleRes(r, res)
			} else if node.Left != nil {
				offset := int(prefixLen)
				prefixLen += 1
				k := merkletree.Key(prefix)
				k.SetBit(false, offset)
				prefix = publicKey(k)
				node = node.Left
				continue
			} else if node.Right != nil {
				offset := int(prefixLen)
				prefixLen += 1
				k := merkletree.Key(prefix)
				k.SetBit(true, offset)
				prefix = publicKey(k)
				node = node.Right
				continue
			} else {
				if prefixLen != merkletree.KeyBits {
					panic("this should never happen")
				}
				if info, isIn := r.infos[prefix]; isIn {
					p.sendAnnounce(r, info.getAnnounce(prefix))
				} else {
					panic("this should never happen")
				}
			}
			break
		}
	})
}

func (r *router) handleMerkleRes(from phony.Actor, p *peer, res *routerMerkleRes) {
	r.Act(from, func() {
		if res.prefixLen == merkletree.KeyBits {
			// This is a response to a full key, we can't ask for children, and there's nothing useful to do with it right now.
			return
		}
		merk := r.merks[p.key]
		if digest, ok := merk.Lookup(merkletree.Key(res.prefix), int(res.prefixLen)); !ok || digest != res.digest {
			// We disagree, so ask about the left and right children
			left := routerMerkleReq{
				prefixLen: res.prefixLen + 1,
				prefix:    publicKey(merkletree.GetLeft(merkletree.Key(res.prefix), int(res.prefixLen))),
			}
			if !left.check() {
				panic("this should never happen")
			}
			p.sendMerkleReq(r, &left)
			right := routerMerkleReq{
				prefixLen: res.prefixLen + 1,
				prefix:    publicKey(merkletree.GetRight(merkletree.Key(res.prefix), int(res.prefixLen))),
			}
			if !right.check() {
				panic("this should never happen")
			}
			p.sendMerkleReq(r, &right)
		}
	})
}

func (r *router) sendTraffic(tr *traffic) {
	// This must be non-blocking, to prevent deadlocks between read/write paths in the encrypted package
	// Basically, WriteTo and ReadFrom can't be allowed to block each other, but they could if we allowed backpressure here
	// There may be a better way to handle this, but it practice it probably won't be an issue (we'll throw the packet in a queue somewhere, or drop it)
	r.Act(nil, func() {
		r.pathfinder._handleTraffic(tr)
	})
}

func (r *router) handleTraffic(from phony.Actor, tr *traffic) {
	r.Act(from, func() {
		if p := r._lookup(tr); p != nil {
			p.sendTraffic(r, tr)
		} else if tr.dest == r.core.crypto.publicKey {
			r.core.pconn.handleTraffic(r, tr)
		} else {
			// Not addressed to us, and we don't know a next hop.
			// The path is broken, so do something about that.
			r.pathfinder._doBroken(tr)
		}
	})
}

func (r *router) _keyLookup(dest publicKey) publicKey {
	// Returns the key that's the closest match to the destination publicKey
	if _, isIn := r.infos[dest]; !isIn {
		// Switch dest to the closest known key, so out-of-band stuff works
		// This would be a hack to make the example code run without modification
		// Long term, TODO remove out-of-band stuff, provide a function to simply look up the closest known node for a given key
		var lowest *publicKey
		var best *publicKey
		for key := range r.infos {
			if lowest == nil || key.less(*lowest) {
				k := key
				lowest = &k
			}
			if best == nil && key.less(dest) {
				k := key
				best = &k
			}
			if key.less(dest) && best.less(key) {
				k := key
				best = &k
			}
		}
		if best == nil {
			best = lowest
		}
		if best == nil {
			//return nil
		} else {
			dest = *best
		}
	}
	return dest
}

func (r *router) _getRootAndDists(dest publicKey) (publicKey, map[publicKey]uint64) {
	// This returns the distances from the destination's root for the destination and each of its ancestors
	// Note that we skip any expired infos
	dists := make(map[publicKey]uint64)
	next := dest
	var root publicKey
	var dist uint64
	for {
		if _, isIn := dists[next]; isIn {
			break
		}
		if info, isIn := r.infos[next]; isIn && !info.expired {
			root = next
			dists[next] = dist
			dist++
			next = info.parent
		} else {
			break
		}
	}
	return root, dists
}

func (r *router) _getRootAndPath(dest publicKey) (publicKey, []peerPort) {
	var ports []peerPort
	visited := make(map[publicKey]struct{})
	var root publicKey
	next := dest
	for {
		if _, isIn := visited[next]; isIn {
			// We hit a loop
			return dest, nil
		}
		if info, isIn := r.infos[next]; isIn && !info.expired {
			root = next
			visited[next] = struct{}{}
			if next == info.parent {
				// We reached a root, don't append the self port (it should be zero anyway)
				break
			}
			ports = append(ports, info.port)
			next = info.parent
		} else {
			// We hit a dead end
			return dest, nil
		}
	}
	// Reverse order, since we built this from the node to the root
	for left, right := 0, len(ports)-1; left < right; left, right = left+1, right-1 {
		ports[left], ports[right] = ports[right], ports[left]
	}
	return root, ports
}

func (r *router) _getDist(destPath []peerPort, key publicKey) uint64 {
	// We cache the keyPath to avoid allocating slices for every lookup
	var keyPath []peerPort
	if cached, isIn := r.cache[key]; isIn {
		keyPath = cached
	} else {
		_, keyPath = r._getRootAndPath(key)
		r.cache[key] = keyPath
	}
	end := len(destPath)
	if len(keyPath) < end {
		end = len(keyPath)
	}
	dist := uint64(len(keyPath) + len(destPath))
	for idx := 0; idx < end; idx++ {
		if keyPath[idx] == destPath[idx] {
			dist -= 2
		} else {
			break
		}
	}
	return dist
}

func (r *router) _lookup(tr *traffic) *peer {
	// Look up the next hop (in treespace) towards the destination
	var bestPeer *peer
	bestDist := ^uint64(0)
	if dist := r._getDist(tr.path, r.core.crypto.publicKey); dist < tr.watermark {
		bestDist = dist // Self dist, so other nodes must be strictly better by distance
		tr.watermark = dist
	} else {
		return nil
	}
	for k, ps := range r.peers {
		if dist := r._getDist(tr.path, k); dist < bestDist {
			for p := range ps {
				switch {
				case bestPeer != nil && p.prio > bestPeer.prio:
					// Skip worse priority links
					continue
				case bestPeer != nil && p.time.After(bestPeer.time):
					// Skip links that have been up for less time
					continue
				default:
					bestPeer = p
				}
			}
			bestDist = dist
		}
	}

	return bestPeer
}

/*****************
 * routerSigReq *
 *****************/

type routerSigReq struct {
	seq   uint64
	nonce uint64
}

func (req *routerSigReq) bytesForSig(node, parent publicKey) []byte {
	out := make([]byte, 0, publicKeySize*2+8+8)
	out = append(out, node[:]...)
	out = append(out, parent[:]...)
	out, _ = req.encode(out)
	return out
}

func (req *routerSigReq) size() int {
	size := wireSizeUint(req.seq)
	size += wireSizeUint(req.nonce)
	return size
}

func (req *routerSigReq) encode(out []byte) ([]byte, error) {
	start := len(out)
	out = wireAppendUint(out, req.seq)
	out = wireAppendUint(out, req.nonce)
	end := len(out)
	if end-start != req.size() {
		panic("this should never happen")
	}
	return out, nil
}

func (req *routerSigReq) chop(data *[]byte) error {
	var tmp routerSigReq
	orig := *data
	if !wireChopUint(&tmp.seq, &orig) {
		return types.ErrDecode
	} else if !wireChopUint(&tmp.nonce, &orig) {
		return types.ErrDecode
	}
	*req = tmp
	*data = orig
	return nil
}

func (req *routerSigReq) decode(data []byte) error {
	var tmp routerSigReq
	if err := tmp.chop(&data); err != nil {
		return err
	} else if len(data) != 0 {
		return types.ErrDecode
	}
	*req = tmp
	return nil
}

/*****************
 * routerSigRes *
 *****************/

type routerSigRes struct {
	routerSigReq
	port peerPort
	psig signature
}

func (res *routerSigRes) check(node, parent publicKey) bool {
	bs := res.bytesForSig(node, parent)
	return parent.verify(bs, &res.psig)
}

func (res *routerSigRes) bytesForSig(node, parent publicKey) []byte {
	bs := res.routerSigReq.bytesForSig(node, parent)
	bs = wireAppendUint(bs, uint64(res.port))
	return bs
}

func (res *routerSigRes) size() int {
	size := res.routerSigReq.size()
	size += wireSizeUint(uint64(res.port))
	size += len(res.psig)
	return size
}

func (res *routerSigRes) encode(out []byte) ([]byte, error) {
	start := len(out)
	var err error
	out, err = res.routerSigReq.encode(out)
	if err != nil {
		return nil, err
	}
	out = wireAppendUint(out, uint64(res.port))
	out = append(out, res.psig[:]...)
	end := len(out)
	if end-start != res.size() {
		panic("this should never happen")
	}
	return out, nil
}

func (res *routerSigRes) chop(data *[]byte) error {
	orig := *data
	var tmp routerSigRes
	if err := tmp.routerSigReq.chop(&orig); err != nil {
		return err
	} else if !wireChopUint((*uint64)(&tmp.port), &orig) {
		return types.ErrDecode
	} else if !wireChopSlice(tmp.psig[:], &orig) {
		return types.ErrDecode
	}
	*res = tmp
	*data = orig
	return nil
}

func (res *routerSigRes) decode(data []byte) error {
	var tmp routerSigRes
	if err := tmp.chop(&data); err != nil {
		return err
	} else if len(data) != 0 {
		return types.ErrDecode
	}
	*res = tmp
	return nil
}

/*******************
 * routerAnnounce *
 *******************/

type routerAnnounce struct {
	key    publicKey
	parent publicKey
	routerSigRes
	sig signature
}

func (ann *routerAnnounce) check() bool {
	if ann.port == 0 && ann.key != ann.parent {
		return false
	}
	bs := ann.bytesForSig(ann.key, ann.parent)
	return ann.key.verify(bs, &ann.sig) && ann.parent.verify(bs, &ann.psig)
}

func (ann *routerAnnounce) size() int {
	size := len(ann.key)
	size += len(ann.parent)
	size += ann.routerSigRes.size()
	size += len(ann.sig)
	return size
}

func (ann *routerAnnounce) encode(out []byte) ([]byte, error) {
	start := len(out)
	var err error
	out = append(out, ann.key[:]...)
	out = append(out, ann.parent[:]...)
	out, err = ann.routerSigRes.encode(out)
	if err != nil {
		return nil, err
	}
	out = append(out, ann.sig[:]...)
	end := len(out)
	if end-start != ann.size() {
		panic("this should never happen")
	}
	return out, nil
}

func (ann *routerAnnounce) decode(data []byte) error {
	var tmp routerAnnounce
	if !wireChopSlice(tmp.key[:], &data) {
		return types.ErrDecode
	} else if !wireChopSlice(tmp.parent[:], &data) {
		return types.ErrDecode
	} else if err := tmp.routerSigRes.chop(&data); err != nil {
		return err
	} else if !wireChopSlice(tmp.sig[:], &data) {
		return types.ErrDecode
	} else if len(data) != 0 {
		return types.ErrDecode
	}
	*ann = tmp
	return nil
}

/***************
 * routerInfo *
 ***************/

// This is the value stored in a key,value map

type routerInfo struct {
	parent publicKey
	routerSigRes
	sig     signature
	expired bool
}

func (info *routerInfo) getAnnounce(key publicKey) *routerAnnounce {
	return &routerAnnounce{
		key:          key,
		parent:       info.parent,
		routerSigRes: info.routerSigRes,
		sig:          info.sig,
	}
}

/*******************
 * routerMerkleReq *
 *******************/

type routerMerkleReq struct {
	prefixLen uint64
	prefix    publicKey
}

func (req *routerMerkleReq) check() bool {
	return req.prefixLen <= merkletree.KeyBits
}

func (req *routerMerkleReq) size() int {
	size := wireSizeUint(req.prefixLen)
	size += len(req.prefix)
	return size
}

func (req *routerMerkleReq) encode(out []byte) ([]byte, error) {
	start := len(out)
	out = wireAppendUint(out, req.prefixLen)
	out = append(out, req.prefix[:]...)
	end := len(out)
	if end-start != req.size() {
		panic("this should never happen")
	}
	return out, nil
}

func (req *routerMerkleReq) chop(data *[]byte) error {
	var tmp routerMerkleReq
	orig := *data
	if !wireChopUint(&tmp.prefixLen, &orig) {
		return types.ErrDecode
	} else if !wireChopSlice(tmp.prefix[:], &orig) {
		return types.ErrDecode
	}
	*req = tmp
	*data = orig
	return nil
}

func (req *routerMerkleReq) decode(data []byte) error {
	var tmp routerMerkleReq
	if err := tmp.chop(&data); err != nil {
		return err
	} else if len(data) != 0 {
		return types.ErrDecode
	}
	*req = tmp
	return nil
}

/*******************
* routerMerkleRes *
*******************/

type routerMerkleRes struct {
	routerMerkleReq
	digest merkletree.Digest
}

func (res *routerMerkleRes) size() int {
	size := res.routerMerkleReq.size()
	size += len(res.digest)
	return size
}

func (res *routerMerkleRes) encode(out []byte) ([]byte, error) {
	start := len(out)
	var err error
	if out, err = res.routerMerkleReq.encode(out); err != nil {
		return nil, err
	}
	out = append(out, res.digest[:]...)
	end := len(out)
	if end-start != res.size() {
		panic("this should never happen")
	}
	return out, nil
}

func (res *routerMerkleRes) chop(data *[]byte) error {
	var tmp routerMerkleRes
	orig := *data
	if err := tmp.routerMerkleReq.chop(&orig); err != nil {
		return err
	} else if !wireChopSlice(tmp.digest[:], &orig) {
		return types.ErrDecode
	}
	*res = tmp
	*data = orig
	return nil
}

func (res *routerMerkleRes) decode(data []byte) error {
	var tmp routerMerkleRes
	if err := tmp.chop(&data); err != nil {
		return err
	} else if len(data) != 0 {
		return types.ErrDecode
	}
	*res = tmp
	return nil
}
