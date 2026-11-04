package fetch

import (
	"bytes"
	"container/heap"
	"errors"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/edusynapse/littlekittyvcs/internal/packstore"
)

// selectObjects returns the objects to send for wants, excluding history
// reachable from common. The result is tags, then commits, then trees, then
// blobs. It may include an object the client already has. It does not leave
// one out unless that object is reachable from common.
func selectObjects(objs *packstore.Objects, wants, common [][20]byte) ([][20]byte, error) {
	w := &walker{
		objs:        objs,
		common:      map[[20]byte]struct{}{},
		known:       map[[20]byte]struct{}{},
		nodes:       map[[20]byte]*cnode{},
		emittedTag:  map[[20]byte]struct{}{},
		emittedTree: map[[20]byte]struct{}{},
		emittedBlob: map[[20]byte]struct{}{},
		treesSeen:   map[[20]byte]*object.Tree{},
	}
	for _, id := range common {
		w.common[id] = struct{}{}
	}
	for _, id := range common {
		if err := w.markCommon(id); err != nil {
			return nil, err
		}
	}
	seenWant := map[[20]byte]struct{}{}
	for _, id := range wants {
		if _, ok := seenWant[id]; ok {
			continue
		}
		seenWant[id] = struct{}{}
		if err := w.followWant(id); err != nil {
			return nil, err
		}
	}
	if err := w.commits(); err != nil {
		return nil, err
	}
	for _, id := range w.sent {
		n := w.nodes[id]
		parents := make([][20]byte, len(n.parents))
		for i, p := range n.parents {
			pn := w.nodes[p]
			if pn == nil || !pn.loaded {
				return nil, errors.New("fetch: parent was not read")
			}
			parents[i] = pn.tree
		}
		if err := w.sendTree(n.tree, parents, map[[20]byte]struct{}{}); err != nil {
			return nil, err
		}
	}
	for _, id := range w.rootTrees {
		if err := w.sendTree(id, nil, map[[20]byte]struct{}{}); err != nil {
			return nil, err
		}
	}
	for _, id := range w.rootBlobs {
		w.addBlob(id)
	}
	out := make([][20]byte, 0, len(w.tags)+len(w.sent)+len(w.trees)+len(w.blobs))
	out = append(out, w.tags...)
	out = append(out, w.sent...)
	out = append(out, w.trees...)
	out = append(out, w.blobs...)
	return out, nil
}

type cnode struct {
	id            [20]byte
	when          int64
	parents       [][20]byte
	tree          [20]byte
	loaded        bool
	uninteresting bool
	queuedU       bool
	queuedI       bool
	sent          bool
}

// cheap is a max-heap by committer time. Equal times pop the smaller id first.
// That direction is a choice the negotiation rules left open.
type cheap struct {
	nodes []*cnode
}

func (h cheap) Len() int { return len(h.nodes) }

func (h cheap) Less(i, j int) bool {
	a, b := h.nodes[i], h.nodes[j]
	if a.when != b.when {
		return a.when > b.when
	}
	return bytes.Compare(a.id[:], b.id[:]) < 0
}

func (h cheap) Swap(i, j int) { h.nodes[i], h.nodes[j] = h.nodes[j], h.nodes[i] }

func (h *cheap) Push(x any) { h.nodes = append(h.nodes, x.(*cnode)) }

func (h *cheap) Pop() any {
	n := len(h.nodes) - 1
	x := h.nodes[n]
	h.nodes[n] = nil
	h.nodes = h.nodes[:n]
	return x
}

func (h cheap) allUninteresting() bool {
	for _, n := range h.nodes {
		if !n.uninteresting {
			return false
		}
	}
	return true
}

type walker struct {
	objs        *packstore.Objects
	common      map[[20]byte]struct{}
	known       map[[20]byte]struct{}
	nodes       map[[20]byte]*cnode
	heap        cheap
	tags        [][20]byte
	sent        [][20]byte
	trees       [][20]byte
	blobs       [][20]byte
	rootTrees   [][20]byte
	rootBlobs   [][20]byte
	emittedTag  map[[20]byte]struct{}
	emittedTree map[[20]byte]struct{}
	emittedBlob map[[20]byte]struct{}
	treesSeen   map[[20]byte]*object.Tree
}

func (w *walker) markCommon(id [20]byte) error {
	peeled, err := packstore.Peel(w.objs, id)
	if err != nil {
		return err
	}
	start := id
	if peeled != nil {
		start = *peeled
	}
	typ, _, err := w.objs.Get(start)
	if err != nil {
		return err
	}
	switch typ {
	case plumbing.CommitObject:
		return w.queue(start, false)
	case plumbing.TreeObject, plumbing.BlobObject:
		w.known[start] = struct{}{}
		return nil
	default:
		return errors.New("fetch: object type")
	}
}

func (w *walker) followWant(id [20]byte) error {
	seen := map[[20]byte]struct{}{}
	for {
		if _, ok := seen[id]; ok {
			return errors.New("fetch: tag cycle")
		}
		seen[id] = struct{}{}
		if _, ok := w.common[id]; ok {
			return nil
		}
		typ, content, err := w.objs.Get(id)
		if err != nil {
			return err
		}
		switch typ {
		case plumbing.TagObject:
			if _, ok := w.emittedTag[id]; !ok {
				w.emittedTag[id] = struct{}{}
				w.tags = append(w.tags, id)
			}
			target, err := readTagTarget(content)
			if err != nil {
				return err
			}
			id = target
		case plumbing.CommitObject:
			return w.queue(id, true)
		case plumbing.TreeObject:
			w.rootTrees = append(w.rootTrees, id)
			return nil
		case plumbing.BlobObject:
			w.rootBlobs = append(w.rootBlobs, id)
			return nil
		default:
			return errors.New("fetch: object type")
		}
	}
}

func (w *walker) queue(id [20]byte, interesting bool) error {
	n := w.nodes[id]
	if n == nil {
		n = &cnode{id: id}
		w.nodes[id] = n
	}
	if !interesting {
		// Uninteresting wins. A commit already queued as interesting is pushed
		// again so the pop sees the flag. It is not read a second time.
		n.uninteresting = true
		if n.queuedU {
			return nil
		}
		n.queuedU = true
		if err := w.load(n); err != nil {
			return err
		}
		heap.Push(&w.heap, n)
		return nil
	}
	if n.uninteresting || n.queuedU || n.queuedI {
		return nil
	}
	n.queuedI = true
	if err := w.load(n); err != nil {
		return err
	}
	heap.Push(&w.heap, n)
	return nil
}

func (w *walker) load(n *cnode) error {
	if n.loaded {
		return nil
	}
	typ, content, err := w.objs.Get(n.id)
	if err != nil {
		return err
	}
	if typ != plumbing.CommitObject {
		return errors.New("fetch: history object is not a commit")
	}
	c, err := readCommit(content)
	if err != nil {
		return err
	}
	n.when = c.Committer.When.Unix()
	n.tree = c.TreeHash
	n.parents = make([][20]byte, len(c.ParentHashes))
	for i, p := range c.ParentHashes {
		n.parents[i] = p
	}
	n.loaded = true
	return nil
}

func (w *walker) commits() error {
	for w.heap.Len() > 0 {
		if w.heap.allUninteresting() {
			return nil
		}
		n := heap.Pop(&w.heap).(*cnode)
		if n.uninteresting {
			for _, p := range n.parents {
				if err := w.queue(p, false); err != nil {
					return err
				}
			}
			continue
		}
		if !n.sent {
			n.sent = true
			w.sent = append(w.sent, n.id)
		}
		for _, p := range n.parents {
			if err := w.queue(p, true); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *walker) sendTree(id [20]byte, parents [][20]byte, visiting map[[20]byte]struct{}) error {
	if _, ok := visiting[id]; ok {
		return errors.New("fetch: tree cycle")
	}
	for _, p := range parents {
		if p == id {
			return nil
		}
	}
	if _, ok := w.known[id]; !ok {
		if _, ok := w.emittedTree[id]; !ok {
			w.emittedTree[id] = struct{}{}
			w.trees = append(w.trees, id)
		}
	}
	tree, err := w.loadTree(id)
	if err != nil {
		return err
	}
	parentTrees := make([]*object.Tree, 0, len(parents))
	for _, p := range parents {
		pt, err := w.loadTree(p)
		if err != nil {
			return err
		}
		parentTrees = append(parentTrees, pt)
	}
	visiting[id] = struct{}{}
	defer delete(visiting, id)
	for _, e := range tree.Entries {
		if parentHas(parentTrees, e.Name, e.Hash) {
			continue
		}
		if e.Mode == filemode.Submodule {
			continue
		}
		if e.Mode == filemode.Dir {
			var subs [][20]byte
			for _, pt := range parentTrees {
				for _, pe := range pt.Entries {
					if pe.Name == e.Name && pe.Mode == filemode.Dir {
						subs = append(subs, [20]byte(pe.Hash))
					}
				}
			}
			if err := w.sendTree([20]byte(e.Hash), subs, visiting); err != nil {
				return err
			}
			continue
		}
		w.addBlob([20]byte(e.Hash))
	}
	return nil
}

func (w *walker) addBlob(id [20]byte) {
	if _, ok := w.known[id]; ok {
		return
	}
	if _, ok := w.emittedBlob[id]; ok {
		return
	}
	w.emittedBlob[id] = struct{}{}
	w.blobs = append(w.blobs, id)
}

func (w *walker) loadTree(id [20]byte) (*object.Tree, error) {
	if t, ok := w.treesSeen[id]; ok {
		return t, nil
	}
	typ, content, err := w.objs.Get(id)
	if err != nil {
		return nil, err
	}
	if typ != plumbing.TreeObject {
		return nil, errors.New("fetch: history object is not a tree")
	}
	mo := &plumbing.MemoryObject{}
	mo.SetType(plumbing.TreeObject)
	if _, err := mo.Write(content); err != nil {
		return nil, err
	}
	var tree object.Tree
	if err := tree.Decode(mo); err != nil {
		return nil, err
	}
	w.treesSeen[id] = &tree
	return &tree, nil
}

func parentHas(parents []*object.Tree, name string, id plumbing.Hash) bool {
	for _, p := range parents {
		for _, e := range p.Entries {
			if e.Name == name && e.Hash == id {
				return true
			}
		}
	}
	return false
}

func readCommit(content []byte) (*object.Commit, error) {
	mo := &plumbing.MemoryObject{}
	mo.SetType(plumbing.CommitObject)
	if _, err := mo.Write(content); err != nil {
		return nil, err
	}
	var c object.Commit
	if err := c.Decode(mo); err != nil {
		return nil, err
	}
	return &c, nil
}

func readTagTarget(content []byte) ([20]byte, error) {
	mo := &plumbing.MemoryObject{}
	mo.SetType(plumbing.TagObject)
	if _, err := mo.Write(content); err != nil {
		return [20]byte{}, err
	}
	var tag object.Tag
	if err := tag.Decode(mo); err != nil {
		return [20]byte{}, err
	}
	return tag.Target, nil
}
