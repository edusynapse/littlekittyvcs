// Package tokenbackup stores an encrypted copy of the tokens table.
// The plaintext of a token is never written. Only the hash is.
package tokenbackup

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

// Backup is one encrypted copy of the tokens table.
type Backup struct {
	ID      [16]byte
	Key     string
	TakenAt time.Time
	Tokens  []store.TokenBackup
}

// RestoreResult is the summary restore-tokens prints.
type RestoreResult struct {
	Restored     int
	Present      int
	NoRepository int
	Unreadable   int
	TakenAt      time.Time
}

// Key is the bucket key of one token backup.
func Key(id [16]byte) string {
	return "tokens/" + store.FormatID(id) + ".tok"
}

// ParseKey reports the uuid of a key that is exactly tokens/<uuid>.tok.
func ParseKey(key string) ([16]byte, bool) {
	const prefix = "tokens/"
	const suffix = ".tok"
	if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, suffix) {
		return [16]byte{}, false
	}
	middle := strings.TrimSuffix(strings.TrimPrefix(key, prefix), suffix)
	id, err := store.ParseID(middle)
	if err != nil {
		return [16]byte{}, false
	}
	return id, true
}

// Write stores one backup under a new id when id is zero.
func Write(ctx context.Context, bucket blob.Store, kr *objcrypt.Keyring, id [16]byte, taken time.Time, tokens []store.TokenBackup) (Backup, error) {
	var zero [16]byte
	if id == zero {
		var err error
		id, err = store.NewID()
		if err != nil {
			return Backup{}, err
		}
	}
	ct, err := seal(kr, id, taken, tokens)
	if err != nil {
		return Backup{}, err
	}
	key := Key(id)
	if err := bucket.Put(ctx, key, bytes.NewReader(ct), int64(len(ct))); err != nil {
		return Backup{}, err
	}
	return Backup{ID: id, Key: key, TakenAt: taken, Tokens: tokens}, nil
}

// WriteCurrent reads the tokens table and stores that backup in every non-nil bucket.
// Each bucket receives the same ciphertext under the same key.
func WriteCurrent(ctx context.Context, st *store.Store, kr *objcrypt.Keyring, buckets ...blob.Store) (Backup, error) {
	taken, rows, err := st.ReadTokensForBackup(ctx)
	if err != nil {
		return Backup{}, err
	}
	id, err := store.NewID()
	if err != nil {
		return Backup{}, err
	}
	ct, err := seal(kr, id, taken, rows)
	if err != nil {
		return Backup{}, err
	}
	key := Key(id)
	var putErr error
	wrote := false
	for _, bucket := range buckets {
		if bucket == nil {
			continue
		}
		if err := bucket.Put(ctx, key, bytes.NewReader(ct), int64(len(ct))); err != nil {
			putErr = errors.Join(putErr, err)
			continue
		}
		wrote = true
	}
	if putErr != nil {
		return Backup{}, putErr
	}
	if !wrote {
		return Backup{}, errors.New("tokenbackup: no bucket")
	}
	return Backup{ID: id, Key: key, TakenAt: taken, Tokens: rows}, nil
}

// Read decrypts one token backup. id is the uuid in its key.
func Read(ctx context.Context, src objcrypt.RangeSource, kr *objcrypt.Keyring, id [16]byte, size int64) (Backup, error) {
	return open(ctx, src, Key(id), size, kr, id)
}

// Newest decrypts every tokens/ object and returns the one with the greatest
// taken_at. Equal times keep the greater key. A key that does not parse, or
// does not decrypt, is counted in unreadable and skipped. An error means no
// backup was readable.
func Newest(ctx context.Context, bucket blob.Store, kr *objcrypt.Keyring) (Backup, int, error) {
	var best *Backup
	unreadable := 0
	err := bucket.List(ctx, "tokens/", func(info blob.Info) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		id, ok := ParseKey(info.Key)
		if !ok {
			unreadable++
			return nil
		}
		got, err := open(ctx, bucket, info.Key, info.Size, kr, id)
		if err != nil {
			unreadable++
			return nil
		}
		if best == nil || newer(got, *best) {
			best = &got
		}
		return nil
	})
	if err != nil {
		return Backup{}, unreadable, err
	}
	if best == nil {
		return Backup{}, unreadable, errors.New("tokenbackup: no readable backup")
	}
	return *best, unreadable, nil
}

// Restore loads the newest token backup from bucket into st.
func Restore(ctx context.Context, st *store.Store, bucket blob.Store, kr *objcrypt.Keyring) (RestoreResult, error) {
	best, unreadable, err := Newest(ctx, bucket, kr)
	if err != nil {
		return RestoreResult{Unreadable: unreadable}, err
	}
	counts, err := st.RestoreTokens(ctx, best.Tokens)
	if err != nil {
		return RestoreResult{}, err
	}
	return RestoreResult{
		Restored:     counts.Restored,
		Present:      counts.Present,
		NoRepository: counts.NoRepository,
		Unreadable:   unreadable,
		TakenAt:      best.TakenAt,
	}, nil
}

func newer(a, b Backup) bool {
	if a.TakenAt.After(b.TakenAt) {
		return true
	}
	if a.TakenAt.Equal(b.TakenAt) && a.Key > b.Key {
		return true
	}
	return false
}

func seal(kr *objcrypt.Keyring, id [16]byte, taken time.Time, tokens []store.TokenBackup) ([]byte, error) {
	plain, err := marshal(id, taken, tokens)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	w, err := objcrypt.NewWriter(&buf, kr, objcrypt.Binding{
		ObjectID: id,
		Kind:     objcrypt.KindTokens,
	}, objcrypt.WriterOptions{})
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func open(ctx context.Context, src objcrypt.RangeSource, key string, size int64, kr *objcrypt.Keyring, id [16]byte) (Backup, error) {
	rd, err := objcrypt.Open(ctx, src, key, size, kr, objcrypt.Binding{
		ObjectID: id,
		Kind:     objcrypt.KindTokens,
	})
	if err != nil {
		return Backup{}, err
	}
	plain := make([]byte, rd.Size())
	n, err := rd.ReadAt(plain, 0)
	if int(n) != len(plain) {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return Backup{}, err
	}
	tokens, taken, err := unmarshal(plain)
	if err != nil {
		return Backup{}, err
	}
	return Backup{ID: id, Key: key, TakenAt: taken, Tokens: tokens}, nil
}

func marshal(id [16]byte, taken time.Time, tokens []store.TokenBackup) ([]byte, error) {
	docs := make([]tokDoc, 0, len(tokens))
	seenID := map[[16]byte]struct{}{}
	seenHash := map[[32]byte]struct{}{}
	for _, t := range tokens {
		if _, ok := seenID[t.ID]; ok {
			return nil, errors.New("tokenbackup: invalid")
		}
		if _, ok := seenHash[t.Hash]; ok {
			return nil, errors.New("tokenbackup: invalid")
		}
		seenID[t.ID] = struct{}{}
		seenHash[t.Hash] = struct{}{}
		if err := validToken(t); err != nil {
			return nil, err
		}
		write := t.Write
		admin := t.Admin
		doc := tokDoc{
			ID:        store.FormatID(t.ID),
			TokenHash: hex.EncodeToString(t.Hash[:]),
			Label:     t.Label,
			Write:     reqBool{set: true, v: write},
			Admin:     reqBool{set: true, v: admin},
			CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339Nano),
		}
		if t.RepoID == nil {
			doc.RepoID = repoJSON{set: true, null: true}
		} else {
			doc.RepoID = repoJSON{set: true, id: *t.RepoID}
		}
		docs = append(docs, doc)
	}
	return json.Marshal(doc{
		Format:  formatV{set: true, n: 1},
		TakenAt: takenAtJSON{set: true, t: taken.UTC()},
		Tokens:  tokenList{set: true, items: docs},
	})
}

func validToken(t store.TokenBackup) error {
	var zero [16]byte
	if t.ID == zero {
		return errors.New("tokenbackup: invalid")
	}
	if n := utf8.RuneCountInString(t.Label); n < 1 || n > 200 {
		return errors.New("tokenbackup: invalid")
	}
	if t.Admin && !t.Write {
		return errors.New("tokenbackup: invalid")
	}
	if t.Admin && t.RepoID != nil {
		return errors.New("tokenbackup: invalid")
	}
	if t.CreatedAt.IsZero() {
		return errors.New("tokenbackup: invalid")
	}
	return nil
}

func unmarshal(plain []byte) ([]store.TokenBackup, time.Time, error) {
	dec := json.NewDecoder(bytes.NewReader(plain))
	dec.DisallowUnknownFields()
	var doc doc
	if err := dec.Decode(&doc); err != nil {
		return nil, time.Time{}, errors.New("tokenbackup: invalid")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, time.Time{}, errors.New("tokenbackup: invalid")
	}
	if !doc.Format.set || doc.Format.n != 1 || !doc.TakenAt.set || !doc.Tokens.set {
		return nil, time.Time{}, errors.New("tokenbackup: invalid")
	}
	out := make([]store.TokenBackup, 0, len(doc.Tokens.items))
	seenID := map[[16]byte]struct{}{}
	seenHash := map[[32]byte]struct{}{}
	for _, item := range doc.Tokens.items {
		row, err := item.row()
		if err != nil {
			return nil, time.Time{}, errors.New("tokenbackup: invalid")
		}
		if _, ok := seenID[row.ID]; ok {
			return nil, time.Time{}, errors.New("tokenbackup: invalid")
		}
		if _, ok := seenHash[row.Hash]; ok {
			return nil, time.Time{}, errors.New("tokenbackup: invalid")
		}
		seenID[row.ID] = struct{}{}
		seenHash[row.Hash] = struct{}{}
		out = append(out, row)
	}
	return out, doc.TakenAt.t, nil
}

type doc struct {
	Format  formatV     `json:"format"`
	TakenAt takenAtJSON `json:"taken_at"`
	Tokens  tokenList   `json:"tokens"`
}

type tokDoc struct {
	ID        string   `json:"id"`
	TokenHash string   `json:"token_hash"`
	Label     string   `json:"label"`
	Write     reqBool  `json:"write"`
	Admin     reqBool  `json:"admin"`
	RepoID    repoJSON `json:"repo_id"`
	CreatedAt string   `json:"created_at"`
}

func (d tokDoc) row() (store.TokenBackup, error) {
	if !d.Write.set || !d.Admin.set || !d.RepoID.set || d.ID == "" || d.TokenHash == "" || d.Label == "" || d.CreatedAt == "" {
		return store.TokenBackup{}, errors.New("tokenbackup: invalid")
	}
	id, err := store.ParseID(d.ID)
	if err != nil {
		return store.TokenBackup{}, err
	}
	if len(d.TokenHash) != 64 {
		return store.TokenBackup{}, errors.New("tokenbackup: invalid")
	}
	raw, err := hex.DecodeString(d.TokenHash)
	if err != nil || len(raw) != 32 {
		return store.TokenBackup{}, errors.New("tokenbackup: invalid")
	}
	var hash [32]byte
	copy(hash[:], raw)
	created, err := time.Parse(time.RFC3339Nano, d.CreatedAt)
	if err != nil {
		return store.TokenBackup{}, err
	}
	row := store.TokenBackup{
		ID:        id,
		Hash:      hash,
		Label:     d.Label,
		Write:     d.Write.v,
		Admin:     d.Admin.v,
		CreatedAt: created,
	}
	if !d.RepoID.null {
		id := d.RepoID.id
		row.RepoID = &id
	}
	if err := validToken(row); err != nil {
		return store.TokenBackup{}, err
	}
	return row, nil
}

type formatV struct {
	set bool
	n   int
}

func (f formatV) MarshalJSON() ([]byte, error) {
	if !f.set {
		return []byte("null"), nil
	}
	return json.Marshal(f.n)
}

func (f *formatV) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return errors.New("tokenbackup: invalid")
	}
	if err := json.Unmarshal(data, &f.n); err != nil {
		return err
	}
	f.set = true
	return nil
}

type takenAtJSON struct {
	set bool
	t   time.Time
}

func (t takenAtJSON) MarshalJSON() ([]byte, error) {
	if !t.set {
		return []byte("null"), nil
	}
	return json.Marshal(t.t.UTC().Format(time.RFC3339Nano))
}

func (t *takenAtJSON) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	t.set = true
	t.t = parsed
	return nil
}

type tokenList struct {
	set   bool
	items []tokDoc
}

func (t tokenList) MarshalJSON() ([]byte, error) {
	if !t.set {
		return []byte("null"), nil
	}
	if t.items == nil {
		t.items = []tokDoc{}
	}
	return json.Marshal(t.items)
}

func (t *tokenList) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return errors.New("tokenbackup: invalid")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var items []tokDoc
	if err := dec.Decode(&items); err != nil {
		return err
	}
	t.set = true
	t.items = items
	return nil
}

type reqBool struct {
	set bool
	v   bool
}

func (b reqBool) MarshalJSON() ([]byte, error) {
	if !b.set {
		return []byte("null"), nil
	}
	return json.Marshal(b.v)
}

func (b *reqBool) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return errors.New("tokenbackup: invalid")
	}
	if err := json.Unmarshal(data, &b.v); err != nil {
		return err
	}
	b.set = true
	return nil
}

// repoJSON is present on every token. JSON null means every repository.
// A missing field leaves set false, which is refused.
type repoJSON struct {
	set  bool
	null bool
	id   [16]byte
}

func (r repoJSON) MarshalJSON() ([]byte, error) {
	if !r.set || r.null {
		return []byte("null"), nil
	}
	return json.Marshal(store.FormatID(r.id))
}

func (r *repoJSON) UnmarshalJSON(data []byte) error {
	r.set = true
	if string(data) == "null" {
		r.null = true
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	id, err := store.ParseID(s)
	if err != nil {
		return err
	}
	r.id = id
	return nil
}

// FormatSummary is the stdout line of restore-tokens.
func FormatSummary(r RestoreResult) string {
	return fmt.Sprintf("restored tokens=%d present=%d no_repository=%d unreadable=%d taken_at=%s",
		r.Restored, r.Present, r.NoRepository, r.Unreadable, r.TakenAt.UTC().Format(time.RFC3339Nano))
}
