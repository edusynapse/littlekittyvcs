package packstore

import "github.com/edusynapse/littlekittyvcs/internal/store"

// PackKey is the bucket key of a pack object.
func PackKey(repoID, packID [16]byte) string {
	return "repos/" + store.FormatID(repoID) + "/packs/" + store.FormatID(packID) + ".pack"
}

// IdxKey is the bucket key of that pack's index object.
func IdxKey(repoID, packID [16]byte) string {
	return "repos/" + store.FormatID(repoID) + "/packs/" + store.FormatID(packID) + ".idx"
}
