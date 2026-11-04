// Package fetch is the git-upload-pack HTTP handler.
// A clone, and a fetch with nothing in common, receives every stored pack
// joined into one. A fetch that names a stored object receives only the
// objects the client is missing.
package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"

	"github.com/edusynapse/littlekittyvcs/internal/blob"
	"github.com/edusynapse/littlekittyvcs/internal/objcrypt"
	"github.com/edusynapse/littlekittyvcs/internal/objindex"
	"github.com/edusynapse/littlekittyvcs/internal/packstore"
	"github.com/edusynapse/littlekittyvcs/internal/store"
)

// maxFetchBody is the largest decoded upload-pack request.
// 10 MiB is about 200,000 have lines.
const maxFetchBody = 10 << 20

// sidebandDataMax is the most pack data one sideband-64k pkt-line can carry
// through go-git v5.19.3. See writeJoined.
const sidebandDataMax = 65515

// Deps are the process resources a fetch uses.
type Deps struct {
	Bucket     blob.Store
	Keyring    *objcrypt.Keyring
	Index      *objindex.Index
	Log        *slog.Logger
	ChunkCache *objcrypt.ChunkCache
}

// Target is the repository being fetched. Repo is the row ReadRepo returned.
type Target struct {
	Repo  store.Repo
	Refs  []store.Ref
	Packs []store.Pack
}

type want struct {
	id   [20]byte
	text string
}

// Handle serves POST /{repo}/git-upload-pack.
func Handle(d Deps, w http.ResponseWriter, r *http.Request, t Target) {
	start := time.Now()
	cw := &countWriter{ResponseWriter: w}
	w = cw
	if mediaType(r.Header.Get("Content-Type")) != "application/x-git-upload-pack-request" {
		text(w, http.StatusUnsupportedMediaType, "unsupported media type\n")
		return
	}
	body, closer, encErr := decodedBody(r)
	if encErr == errEncoding {
		text(w, http.StatusUnsupportedMediaType, "unsupported content encoding\n")
		return
	}
	if encErr != nil {
		text(w, http.StatusBadRequest, "invalid request\n")
		return
	}
	if closer != nil {
		defer closer.Close()
	}
	data, err := io.ReadAll(io.LimitReader(body, maxFetchBody+1))
	if err != nil || len(data) > maxFetchBody {
		text(w, http.StatusBadRequest, "invalid request\n")
		return
	}
	// Same probe as receive-pack: git 2.34.1 posts "0000" before an upload-pack
	// body that does not fit in http.postBuffer, and continues only on HTTP 200.
	if bytes.Equal(data, []byte("0000")) {
		resultHeaders(w)
		return
	}
	wants, haves, done, side, multi, err := parseRequest(data)
	if errors.Is(err, errShallow) {
		text(w, http.StatusBadRequest, "shallow and partial clone are not supported\n")
		return
	}
	if err != nil {
		text(w, http.StatusBadRequest, "invalid request\n")
		return
	}
	if id, ok := unknownWant(wants, t.Refs); !ok {
		resultHeaders(w)
		_ = pktline.NewEncoder(w).EncodeString("ERR upload-pack: not our ref " + id + "\n")
		return
	}
	var common [][20]byte
	if len(haves) > 0 && multi {
		if d.Index == nil {
			d.log(errors.New("fetch: no object index"), t.Repo.ID)
			text(w, http.StatusInternalServerError, "internal error\n")
			return
		}
		if err := d.Index.Refresh(r.Context(), d.Bucket, d.Keyring, t.Repo.ID, t.Packs); err != nil {
			d.log(err, t.Repo.ID)
			text(w, http.StatusInternalServerError, "internal error\n")
			return
		}
		for _, id := range haves {
			if _, _, ok := d.Index.Lookup(t.Repo.ID, id); ok {
				common = append(common, id)
			}
		}
	}
	if done && multi && len(common) > 0 {
		d.writeGenerated(w, r, t, wants, common, side, start)
		return
	}
	var parts []packPart
	var sum uint64
	if done {
		parts, sum, err = openPacks(r.Context(), d, t)
		if err != nil {
			d.log(err, t.Repo.ID)
			text(w, http.StatusInternalServerError, "internal error\n")
			return
		}
	}
	resultHeaders(w)
	enc := pktline.NewEncoder(w)
	if !done && multi {
		for _, id := range common {
			_ = enc.EncodeString("ACK " + hex.EncodeToString(id[:]) + " common\n")
		}
	}
	_ = enc.EncodeString("NAK\n")
	if !done {
		return
	}
	var dst io.Writer = w
	var mux *sideband.Muxer
	if side {
		mux = sideband.NewMuxer(sideband.Sideband64k, w)
		dst = mux
	}
	if err := writeJoined(dst, parts, sum); err != nil {
		d.log(err, t.Repo.ID)
		if mux != nil {
			_, _ = mux.WriteChannel(sideband.ErrorMessage, []byte("internal error\n"))
		}
		return
	}
	if mux != nil {
		_ = pktline.NewEncoder(w).Flush()
	}
	var reads, hits int64
	for _, part := range parts {
		reads += part.rd.RangeReads()
		hits += part.rd.CacheHits()
	}
	d.served(t.Repo.ID, "joined", len(parts), reads, hits, cw.n, start)
}

func (d Deps) writeGenerated(w http.ResponseWriter, r *http.Request, t Target, wants []want, common [][20]byte, side bool, start time.Time) {
	objs := packstore.NewObjects(r.Context(), d.Bucket, d.Keyring, t.Repo.ID, func(id [20]byte) (store.Pack, *idxfile.MemoryIndex, bool) {
		return d.Index.Lookup(t.Repo.ID, id)
	}, nil, d.ChunkCache)
	defer objs.Close()
	wantIDs := make([][20]byte, len(wants))
	for i, want := range wants {
		wantIDs[i] = want.id
	}
	ids, err := selectObjects(objs, wantIDs, common)
	if err != nil {
		d.log(err, t.Repo.ID)
		text(w, http.StatusInternalServerError, "internal error\n")
		return
	}
	resultHeaders(w)
	enc := pktline.NewEncoder(w)
	var last [20]byte
	for _, id := range common {
		_ = enc.EncodeString("ACK " + hex.EncodeToString(id[:]) + " common\n")
		last = id
	}
	_ = enc.EncodeString("ACK " + hex.EncodeToString(last[:]) + "\n")
	var dst io.Writer = w
	var mux *sideband.Muxer
	if side {
		mux = sideband.NewMuxer(sideband.Sideband64k, w)
		dst = mux
	}
	if err := writeGenerated(dst, objs, ids); err != nil {
		d.log(err, t.Repo.ID)
		if mux != nil {
			_, _ = mux.WriteChannel(sideband.ErrorMessage, []byte("internal error\n"))
		}
		return
	}
	if mux != nil {
		_ = pktline.NewEncoder(w).Flush()
	}
	d.served(t.Repo.ID, "generated", objs.OpenedPacks(), objs.RangeReads(), objs.CacheHits(), bytesWritten(w), start)
}

var objectIDLog = regexp.MustCompile(`[0-9a-fA-F]{40}`)

func (d Deps) log(err error, repoID [16]byte) {
	if d.Log == nil || err == nil {
		return
	}
	msg := objectIDLog.ReplaceAllString(err.Error(), "object")
	d.Log.Error("fetch", "err", errors.New(msg), "repository", store.FormatID(repoID))
}

var errEncoding = errors.New("fetch: unsupported content encoding")
var errShallow = errors.New("fetch: shallow")

func decodedBody(r *http.Request) (io.Reader, io.Closer, error) {
	enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	switch enc {
	case "", "identity":
		return r.Body, nil, nil
	case "gzip":
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, nil, err
		}
		return zr, zr, nil
	default:
		return nil, nil, errEncoding
	}
}

func parseRequest(data []byte) (wants []want, haves [][20]byte, done, side, multi bool, err error) {
	scan := pktline.NewScanner(bytes.NewReader(data))
	phase := phaseWants
	seenHave := map[[20]byte]struct{}{}
	for scan.Scan() {
		payload := scan.Bytes()
		if len(payload) == 0 {
			switch phase {
			case phaseWants:
				if len(wants) == 0 {
					return nil, nil, false, false, false, errors.New("fetch: no wants")
				}
				phase = phaseHaves
			case phaseHaves:
				continue
			default:
				return nil, nil, false, false, false, errors.New("fetch: data after done")
			}
			continue
		}
		line := strings.TrimRight(string(payload), "\n")
		word, rest, _ := strings.Cut(line, " ")
		switch word {
		case "shallow", "deepen", "deepen-since", "deepen-not", "filter":
			return nil, nil, false, false, false, errShallow
		case "want":
			if phase != phaseWants {
				return nil, nil, false, false, false, errors.New("fetch: want after flush")
			}
			fields := strings.Fields(rest)
			if len(fields) == 0 {
				return nil, nil, false, false, false, errors.New("fetch: want")
			}
			id, ok := parseID(fields[0])
			if !ok {
				return nil, nil, false, false, false, errors.New("fetch: want id")
			}
			if len(wants) == 0 {
				for _, cap := range fields[1:] {
					switch cap {
					case "side-band-64k":
						side = true
					case "multi_ack_detailed":
						multi = true
					}
				}
			}
			wants = append(wants, want{id: id, text: fields[0]})
		case "have":
			if phase != phaseHaves {
				return nil, nil, false, false, false, errors.New("fetch: have before flush")
			}
			idText, _, _ := strings.Cut(rest, " ")
			id, ok := parseID(idText)
			if !ok {
				return nil, nil, false, false, false, errors.New("fetch: have id")
			}
			if _, ok := seenHave[id]; !ok {
				seenHave[id] = struct{}{}
				haves = append(haves, id)
			}
		case "done":
			if phase != phaseHaves || rest != "" {
				return nil, nil, false, false, false, errors.New("fetch: done")
			}
			phase = phaseDone
			done = true
		default:
			return nil, nil, false, false, false, errors.New("fetch: unknown line")
		}
	}
	if scan.Err() != nil || phase == phaseWants {
		return nil, nil, false, false, false, errors.New("fetch: bad pkt-line")
	}
	return wants, haves, done, side, multi, nil
}

const (
	phaseWants = iota
	phaseHaves
	phaseDone
)

func unknownWant(wants []want, refs []store.Ref) (string, bool) {
	have := map[[20]byte]struct{}{}
	for _, ref := range refs {
		if ref.ObjectID != nil {
			have[*ref.ObjectID] = struct{}{}
		}
	}
	for _, w := range wants {
		if _, ok := have[w.id]; !ok {
			return w.text, false
		}
	}
	return "", true
}

func parseID(s string) ([20]byte, bool) {
	var id [20]byte
	if len(s) != 40 {
		return id, false
	}
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 20 {
		return id, false
	}
	copy(id[:], raw)
	return id, true
}

type packPart struct {
	rd *objcrypt.Reader
}

func openPacks(ctx context.Context, d Deps, t Target) ([]packPart, uint64, error) {
	parts := make([]packPart, 0, len(t.Packs))
	var sum uint64
	insert := joinedInsert(d.ChunkCache, t.Packs)
	for _, p := range t.Packs {
		rd, err := d.ChunkCache.Open(ctx, d.Bucket, p.PackKey, p.Bytes, d.Keyring, objcrypt.Binding{
			RepoID: t.Repo.ID, ObjectID: p.ID, Kind: objcrypt.KindPack,
		}, insert)
		if err != nil {
			return nil, 0, err
		}
		if rd.Size() < 32 {
			return nil, 0, errors.New("fetch: pack is too small")
		}
		var hdr [12]byte
		n, err := rd.ReadAt(hdr[:], 0)
		if n != len(hdr) {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return nil, 0, err
		}
		if string(hdr[:4]) != "PACK" {
			return nil, 0, errors.New("fetch: bad pack header")
		}
		// A stored version 3 pack is copied under a version 2 joined header.
		// For a SHA-1 repository the object encoding of versions 2 and 3 is
		// the same. See dev_plans/piece_04_push_and_clone.md.
		version := binary.BigEndian.Uint32(hdr[4:8])
		if version != 2 && version != 3 {
			return nil, 0, errors.New("fetch: unsupported pack version")
		}
		sum += uint64(binary.BigEndian.Uint32(hdr[8:12]))
		parts = append(parts, packPart{rd: rd})
	}
	if sum > math.MaxUint32 {
		return nil, 0, errors.New("fetch: object count does not fit a pack header")
	}
	return parts, sum, nil
}

func writeJoined(dst io.Writer, parts []packPart, sum uint64) error {
	h := sha1.New()
	mw := io.MultiWriter(dst, h)
	var hdr [12]byte
	copy(hdr[:4], "PACK")
	binary.BigEndian.PutUint32(hdr[4:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], uint32(sum))
	if _, err := mw.Write(hdr[:]); err != nil {
		return err
	}
	// go-git v5.19.3 Muxer splits a Write at MaxPackedSize64k-1 (65519).
	// The channel byte makes a 65520-byte payload, and pktline.Encoder
	// rejects anything over MaxPayloadSize (65516). Stay at 65515.
	buf := make([]byte, sidebandDataMax)
	for _, part := range parts {
		// Bytes [12, plaintext size-20) are this pack's objects. Offset deltas
		// point backwards inside the pack, and the objects stay contiguous.
		sec := io.NewSectionReader(part.rd, 12, part.rd.Size()-32)
		if _, err := io.CopyBuffer(mw, sec, buf); err != nil {
			return err
		}
	}
	_, err := dst.Write(h.Sum(nil))
	return err
}

// joinedInsert is true when a joined response may add chunks.
// A repository larger than a quarter of the cache would evict everything
// it had just read, so those clones only look up.
func joinedInsert(c *objcrypt.ChunkCache, packs []store.Pack) bool {
	if c == nil {
		return false
	}
	var total int64
	for _, p := range packs {
		total += p.Bytes
	}
	return total <= c.MaxBytes()/4
}

func (d Deps) served(repoID [16]byte, mode string, packs int, reads, hits, bytes int64, start time.Time) {
	if d.Log == nil {
		return
	}
	d.Log.Info("fetch served",
		"repository", store.FormatID(repoID),
		"mode", mode,
		"packs", packs,
		"range_reads", reads,
		"cache_hits", hits,
		"bytes", bytes,
		"ms", time.Since(start).Milliseconds(),
	)
}

type countWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

func (c *countWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func bytesWritten(w http.ResponseWriter) int64 {
	cw, ok := w.(*countWriter)
	if !ok {
		return 0
	}
	return cw.n
}

func resultHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
}

func text(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func mediaType(v string) string {
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = v[:i]
	}
	return strings.ToLower(strings.TrimSpace(v))
}
