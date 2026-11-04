package packstore

import (
	"errors"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Refs returns every object id obj points at.
// A tree entry of mode 160000 is a submodule commit in another repository, so it is skipped.
func Refs(t plumbing.ObjectType, content []byte) ([][20]byte, error) {
	mo := &plumbing.MemoryObject{}
	mo.SetType(t)
	if _, err := mo.Write(content); err != nil {
		return nil, err
	}
	switch t {
	case plumbing.CommitObject:
		var c object.Commit
		if err := c.Decode(mo); err != nil {
			return nil, err
		}
		out := make([][20]byte, 0, 1+len(c.ParentHashes))
		out = append(out, c.TreeHash)
		for _, parent := range c.ParentHashes {
			out = append(out, parent)
		}
		return out, nil
	case plumbing.TreeObject:
		var tree object.Tree
		if err := tree.Decode(mo); err != nil {
			return nil, err
		}
		var out [][20]byte
		for _, entry := range tree.Entries {
			if entry.Mode == filemode.Submodule {
				continue
			}
			out = append(out, entry.Hash)
		}
		return out, nil
	case plumbing.TagObject:
		var tag object.Tag
		if err := tag.Decode(mo); err != nil {
			return nil, err
		}
		return [][20]byte{tag.Target}, nil
	case plumbing.BlobObject:
		return nil, nil
	default:
		return nil, errors.New("packstore: unknown object type")
	}
}
