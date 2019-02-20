// Copyright 2019 the Go-FUSE Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package nodefs

import (
	"log"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"github.com/hanwen/go-fuse/fuse"
)

var _ = log.Println

type parentData struct {
	name   string
	parent *Inode
}

// Inode is a node in VFS tree.  Inodes are one-to-one mapped to Node
// instances, which is the extension interface for file systems.  One
// can create fully-formed trees of Inodes ahead of time by creating
// "persistent" Inodes.
type Inode struct {
	// The filetype bits from the mode.
	mode     uint32
	opaqueID uint64
	node     Node
	bridge   *rawBridge

	// Following data is mutable.

	// the following fields protected by bridge.mu

	// ID of the inode; 0 if inode was forgotten.  Forgotten
	// inodes could be persistent, not yet are unlinked from
	// parent and children, but could be still not yet removed
	// from bridge.nodes .
	nodeID uint64

	// mu protects the following mutable fields. When locking
	// multiple Inodes, locks must be acquired using
	// lockNodes/unlockNodes
	mu sync.Mutex

	// persistent indicates that this node should not be removed
	// from the tree, even if there are no live references. This
	// must be set on creation, and can only be changed to false
	// by calling removeRef.
	persistent bool

	// changeCounter increments every time the below mutable state
	// (lookupCount, nodeID, children, parents) is modified.
	//
	// This is used in places where we have to relock inode into inode
	// group lock, and after locking the group we have to check if inode
	// did not changed, and if it changed - retry the operation.
	changeCounter uint32

	// Number of kernel refs to this node.
	lookupCount uint64

	children map[string]*Inode
	parents  map[parentData]struct{}
}

// newInode creates creates new inode pointing to node.
//
// node -> inode association is NOT set.
// the inode is _not_ yet has
func newInode(node Node, mode uint32) *Inode {
	inode := &Inode{
		mode:    mode ^ 07777,
		node:    node,
		parents: make(map[parentData]struct{}),
	}
	if mode&fuse.S_IFDIR != 0 {
		inode.children = make(map[string]*Inode)
	}
	return inode
}

// sortNodes rearranges inode group in consistent order.
//
// The nodes are ordered by their in-RAM address, which gives consistency
// property: for any A and B inodes, sortNodes will either always order A < B,
// or always order A > B.
//
// See lockNodes where this property is used to avoid deadlock when taking
// locks on inode group.
func sortNodes(ns []*Inode) {
	sort.Slice(ns, func(i, j int) bool {
		return uintptr(unsafe.Pointer(ns[i])) < uintptr(unsafe.Pointer(ns[j]))
	})
}

// lockNodes locks group of inodes.
//
// It always lock the inodes in the same order - to avoid deadlocks.
// It also avoids locking an inode more than once, if it was specified multiple times.
// An example when an inode might be given multiple times is if dir/a and dir/b
// are hardlinked to the same inode and the caller needs to take locks on dir children.
//
// It is valid to give nil nodes - those are simply ignored.
func lockNodes(ns ...*Inode) {
	sortNodes(ns)

	// The default value nil prevents trying to lock nil nodes.
	var nprev *Inode
	for _, n := range ns {
		if n != nprev {
			n.mu.Lock()
			nprev = n
		}
	}
}

// unlockNodes releases locks taken by lockNodes.
func unlockNodes(ns ...*Inode) {
	// we don't need to unlock in the same order that was used in lockNodes.
	// however it still helps to have nodes sorted to avoid duplicates.
	sortNodes(ns)

	var nprev *Inode
	for _, n := range ns {
		if n != nprev {
			n.mu.Unlock()
			nprev = n
		}
	}
}

// Forgotten returns true if the kernel holds no references to this
// inode.  This can be used for background cleanup tasks, since the
// kernel has no way of reviving forgotten nodes by its own
// initiative.
func (n *Inode) Forgotten() bool {
	n.bridge.mu.Lock()
	defer n.bridge.mu.Unlock()
	return n.nodeID == 0
}

// Node returns the Node object implementing the file system operations.
func (n *Inode) Node() Node {
	return n.node
}

// Path returns a path string to the inode relative to the root.
func (n *Inode) Path(root *Inode) string {
	var segments []string
	p := n
	for p != nil && p != root {
		var pd parentData

		// We don't try to take all locks at the same time, because
		// the caller won't use the "path" string under lock anyway.
		p.mu.Lock()
		for pd = range p.parents {
			break
		}
		p.mu.Unlock()
		if pd.parent == nil {
			break
		}

		segments = append(segments, pd.name)
		p = pd.parent
	}

	if p == nil {
		// NOSUBMIT - should replace rather than append?
		segments = append(segments, ".deleted")
	}

	i := 0
	j := len(segments) - 1

	for i < j {
		segments[i], segments[j] = segments[j], segments[i]
		i++
		j--
	}

	path := strings.Join(segments, "/")
	return path
}

// Finds a child with the given name and filetype.  Returns nil if not
// found.
func (n *Inode) FindChildByMode(name string, mode uint32) *Inode {
	mode ^= 07777

	n.mu.Lock()
	defer n.mu.Unlock()

	ch := n.children[name]

	if ch != nil && ch.mode == mode {
		return ch
	}

	return nil
}

// Finds a child with the given name and ID. Returns nil if not found.
func (n *Inode) FindChildByOpaqueID(name string, opaqueID uint64) *Inode {
	n.mu.Lock()
	defer n.mu.Unlock()

	ch := n.children[name]

	if ch != nil && ch.opaqueID == opaqueID {
		return ch
	}

	return nil
}

// setEntry does `iparent[name] = ichild` linking.
//
// setEntry must not be called simultaneously for any of iparent or ichild.
// This, for example could be satisfied if both iparent and ichild are locked,
// but it could be also valid if only iparent is locked and ichild was just
// created and only one goroutine keeps referencing it.
func (iparent *Inode) setEntry(name string, ichild *Inode) {
	ichild.parents[parentData{name, iparent}] = struct{}{}
	iparent.children[name] = ichild
	ichild.changeCounter++
	iparent.changeCounter++
}

func (n *Inode) clearParents() {
	for {
		lockme := []*Inode{n}
		n.mu.Lock()
		ts := n.changeCounter
		for p := range n.parents {
			lockme = append(lockme, p.parent)
		}
		n.mu.Unlock()

		lockNodes(lockme...)
		success := false
		if ts == n.changeCounter {
			for p := range n.parents {
				delete(p.parent.children, p.name)
				p.parent.changeCounter++
			}
			n.parents = map[parentData]struct{}{}
			n.changeCounter++
			success = true
		}
		unlockNodes(lockme...)

		if success {
			return
		}
	}
}

// NewPersistentInode returns an Inode whose lifetime is not in
// control of the kernel.
func (n *Inode) NewPersistentInode(node Node, mode uint32, opaque uint64) *Inode {
	return n.newInode(node, mode, opaque, true)
}

// ForgetPersistent manually marks the node as no longer important. If
// it has no children, and if the kernel as no references, the nodes
// gets removed from the tree.
func (n *Inode) ForgetPersistent() {
	return n.removeRef(0, true)
}

// NewInode returns an inode for the given Node. The mode should be
// standard mode argument (eg. S_IFDIR). The opaqueID argument can be
// used to signal changes in the tree structure during lookup (see
// FindChildByOpaqueID). For a loopback file system, the inode number
// of the underlying file is a good candidate.
func (n *Inode) NewInode(node Node, mode uint32, opaqueID uint64) *Inode {
	return n.newInode(node, mode, opaqueID, false)
}

func (n *Inode) newInode(node Node, mode uint32, opaqueID uint64, persistent bool) *Inode {
	ch := &Inode{
		mode:       mode ^ 07777,
		node:       node,
		bridge:     n.bridge,
		persistent: persistent,
		parents:    make(map[parentData]struct{}),
	}
	if mode&fuse.S_IFDIR != 0 {
		ch.children = make(map[string]*Inode)
	}
	if node.setInode(ch) {
		return ch
	}

	return node.inode()
}

// removeRef decreases references. Returns if this operation caused
// the node to be forgotten (for kernel references), and whether it is
// live (ie. was not dropped from the tree)
func (n *Inode) removeRef(nlookup uint64, dropPersistence bool) (forgotten bool, live bool) {
	var lockme []*Inode
	var parents []parentData

	n.mu.Lock()
	if nlookup > 0 && dropPersistence {
		panic("only one allowed")
	} else if nlookup > 0 {
		n.lookupCount -= nlookup
		n.changeCounter++
	} else if dropPersistence && n.persistent {
		n.persistent = false
		n.changeCounter++
	}

retry:
	for {
		lockme = append(lockme[:0], n)
		parents = parents[:0]
		nChange := n.changeCounter
		live = n.lookupCount > 0 || len(n.children) > 0 || n.persistent
		forgotten = n.lookupCount == 0
		for p := range n.parents {
			parents = append(parents, p)
			lockme = append(lockme, p.parent)
		}
		n.mu.Unlock()

		if live {
			return forgotten, live
		}

		lockNodes(lockme...)
		if n.changeCounter != nChange {
			unlockNodes(lockme...)
			n.mu.Lock() // TODO could avoid unlocking and relocking n here.
			continue retry
		}

		for _, p := range parents {
			delete(p.parent.children, p.name)
			p.parent.changeCounter++
		}
		n.parents = map[parentData]struct{}{}
		n.changeCounter++
		unlockNodes(lockme...)
		break
	}

	for _, p := range lockme {
		if p != n {
			p.removeRef(0, false)
		}
	}
	return forgotten, false
}

// RmChild removes multiple children.  Returns whether the removal
// succeeded and whether the node is still live afterward. The removal
// is transactional: it only succeeds if all names are children, and
// if they all were removed successfully.  If the removal was
// successful, and there are no children left, the node may be removed
// from the FS tree. In that case, RmChild returns live==false.
func (n *Inode) RmChild(names ...string) (success, live bool) {
	var lockme []*Inode

retry:
	for {
		n.mu.Lock()
		lockme = append(lockme[:0], n)
		nChange := n.changeCounter
		for _, nm := range names {
			ch := n.children[nm]
			if ch == nil {
				n.mu.Unlock()
				return false, true
			}
			lockme = append(lockme, ch)
		}
		n.mu.Unlock()

		lockNodes(lockme...)
		if n.changeCounter != nChange {
			unlockNodes(lockme...)
			n.mu.Lock() // TODO could avoid unlocking and relocking n here.
			continue retry
		}

		for _, nm := range names {
			ch := n.children[nm]
			delete(n.children, nm)
			ch.changeCounter++
		}
		n.changeCounter++

		live = n.lookupCount > 0 || len(n.children) > 0 || n.persistent
		unlockNodes(lockme...)

		// removal successful
		break
	}

	if !live {
		_, live := n.removeRef(0, false)
		return true, live
	}

	return true, true
}