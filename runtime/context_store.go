package runtime

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ContextStore is a user-space CAS and transactional message-page store. Hashes
// identify content; ownership and context membership authorize access.
type ContextStore struct {
	mu sync.Mutex
	db *sql.DB
}

type ContextPage struct {
	Hash  string `json:"hash"`
	Bytes int64  `json:"bytes"`
}

type ContextSnapshot struct {
	ContextID string        `json:"context_id"`
	Prefix    []ContextPage `json:"prefix"`
	Tail      *ContextPage  `json:"tail,omitempty"`
}

type ContextStats struct {
	Contexts     int   `json:"contexts"`
	References   int   `json:"references"`
	UniqueBlobs  int   `json:"unique_blobs"`
	SharedBlobs  int   `json:"shared_blobs"`
	LogicalBytes int64 `json:"logical_bytes"`
	UniqueBytes  int64 `json:"unique_bytes"`
	SavedBytes   int64 `json:"saved_bytes"`
}

type LSFSCommit struct {
	Cursor         int64  `json:"cursor"`
	ContextID      string `json:"context_id"`
	Kind           string `json:"kind"`
	Handle         string `json:"handle"`
	IdempotencyKey string `json:"idempotency_key"`
}

var (
	ErrContextAccess      = errors.New("context access denied")
	ErrContextCorrupt     = errors.New("context blob integrity check failed")
	ErrContextIdempotency = errors.New("context idempotency key reused with different input")
)

func OpenContextStore(root string) (*ContextStore, error) {
	if root == "" {
		return nil, errors.New("context store directory required")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("context store requires a private directory")
	}
	path := filepath.Join(root, "contexts.db")
	if info, err = os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0) {
		return nil, errors.New("context database requires a private regular file")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS blobs(hash TEXT PRIMARY KEY, data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS contexts(id TEXT PRIMARY KEY, owner TEXT NOT NULL, tail TEXT REFERENCES blobs(hash));
CREATE TABLE IF NOT EXISTS pages(context_id TEXT REFERENCES contexts(id), position INTEGER NOT NULL, hash TEXT REFERENCES blobs(hash), PRIMARY KEY(context_id,position));
CREATE TABLE IF NOT EXISTS grants(context_id TEXT REFERENCES contexts(id), hash TEXT REFERENCES blobs(hash), kind TEXT NOT NULL, PRIMARY KEY(context_id,hash,kind));
CREATE TABLE IF NOT EXISTS operations(context_id TEXT REFERENCES contexts(id), key TEXT NOT NULL, digest TEXT NOT NULL, result TEXT NOT NULL, PRIMARY KEY(context_id,key));
CREATE TABLE IF NOT EXISTS commits(cursor INTEGER PRIMARY KEY AUTOINCREMENT, context_id TEXT REFERENCES contexts(id), kind TEXT NOT NULL, handle TEXT NOT NULL, key TEXT NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &ContextStore{db: db}, nil
}

func (s *ContextStore) Close() error { s.mu.Lock(); defer s.mu.Unlock(); return s.db.Close() }

func contextHash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func contextBlob(tx *sql.Tx, hash string) ([]byte, error) {
	var data []byte
	if err := tx.QueryRow("SELECT data FROM blobs WHERE hash=?", hash).Scan(&data); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrContextCorrupt, err)
	}
	if contextHash(data) != hash {
		return nil, ErrContextCorrupt
	}
	return data, nil
}

func putContextBlob(tx *sql.Tx, data []byte) (string, error) {
	if data == nil {
		data = []byte{}
	}
	hash := contextHash(data)
	if _, err := tx.Exec("INSERT OR IGNORE INTO blobs(hash,data) VALUES(?,?)", hash, data); err != nil {
		return "", err
	}
	_, err := contextBlob(tx, hash)
	return hash, err
}

func authorizeContext(tx *sql.Tx, owner, id string) error {
	var actual string
	if owner == "" || tx.QueryRow("SELECT owner FROM contexts WHERE id=?", id).Scan(&actual) != nil || actual != owner {
		return ErrContextAccess
	}
	return nil
}

func newContextID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *ContextStore) Create(owner string, prefix []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(owner) == "" {
		return "", ErrContextAccess
	}
	id, err := newContextID()
	if err != nil {
		return "", err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO contexts(id,owner) VALUES(?,?)", id, owner); err != nil {
		return "", err
	}
	if len(prefix) > 0 {
		hash, err := putContextBlob(tx, prefix)
		if err != nil {
			return "", err
		}
		if _, err = tx.Exec("INSERT INTO pages VALUES(?,?,?)", id, 0, hash); err != nil {
			return "", err
		}
	}
	return id, tx.Commit()
}

// mutate atomically binds an idempotency key to both its input and result.
func (s *ContextStore) mutate(owner, id, key, kind string, input []byte, fn func(*sql.Tx) (string, error)) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key == "" || len(key) > 1024 {
		return "", errors.New("bounded idempotency key required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err = authorizeContext(tx, owner, id); err != nil {
		return "", err
	}
	digest := contextHash(append([]byte(kind+"\x00"), input...))
	var oldDigest, result string
	err = tx.QueryRow("SELECT digest,result FROM operations WHERE context_id=? AND key=?", id, key).Scan(&oldDigest, &result)
	if err == nil {
		if oldDigest != digest {
			return "", ErrContextIdempotency
		}
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	result, err = fn(tx)
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec("INSERT INTO operations VALUES(?,?,?,?)", id, key, digest, result); err != nil {
		return "", err
	}
	if _, err = tx.Exec("INSERT INTO commits(context_id,kind,handle,key) VALUES(?,?,?,?)", id, kind, result, key); err != nil {
		return "", err
	}
	return result, tx.Commit()
}

func (s *ContextStore) Append(owner, id, key string, data []byte) (string, error) {
	return s.mutate(owner, id, key, "append", data, func(tx *sql.Tx) (string, error) {
		var tail sql.NullString
		if err := tx.QueryRow("SELECT tail FROM contexts WHERE id=?", id).Scan(&tail); err != nil {
			return "", err
		}
		var previous []byte
		if tail.Valid {
			var err error
			previous, err = contextBlob(tx, tail.String)
			if err != nil {
				return "", err
			}
		}
		hash, err := putContextBlob(tx, append(previous, data...))
		if err != nil {
			return "", err
		}
		_, err = tx.Exec("UPDATE contexts SET tail=? WHERE id=?", hash, id)
		return hash, err
	})
}

// Fork seals the parent's current tail into its immutable prefix. Both contexts
// reference the same CAS pages; later appends allocate independent tails.
func (s *ContextStore) Fork(owner, id, key string) (string, error) {
	return s.ForkTo(owner, id, owner, key)
}

// ForkTo seals the parent context and creates a child owned by childOwner.
// The child's pages reference the same CAS blobs (copy-on-write fan-out);
// only the divergence after the fork allocates new content.
func (s *ContextStore) ForkTo(parentOwner, parentID, childOwner, key string) (string, error) {
	if strings.TrimSpace(childOwner) == "" {
		return "", ErrContextAccess
	}
	return s.mutate(parentOwner, parentID, key, "fork", nil, func(tx *sql.Tx) (string, error) {
		child, err := newContextID()
		if err != nil {
			return "", err
		}
		if _, err = tx.Exec("INSERT INTO contexts(id,owner) VALUES(?,?)", child, childOwner); err != nil {
			return "", err
		}
		if _, err = tx.Exec(`INSERT INTO pages(context_id,position,hash)
SELECT id,(SELECT COALESCE(MAX(position)+1,0) FROM pages WHERE context_id=?),tail FROM contexts WHERE id=? AND tail IS NOT NULL`, parentID, parentID); err != nil {
			return "", err
		}
		if _, err = tx.Exec("UPDATE contexts SET tail=NULL WHERE id=?", parentID); err != nil {
			return "", err
		}
		_, err = tx.Exec("INSERT INTO pages SELECT ?,position,hash FROM pages WHERE context_id=?", child, parentID)
		return child, err
	})
}

// Pages returns the sealed page hashes of a context in order plus the
// current unsealed tail hash, if any. Identical page lists are the evidence
// that forked contexts share CAS content.
func (s *ContextStore) Pages(owner, id string) (pages []string, tail string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	if err = authorizeContext(tx, owner, id); err != nil {
		return nil, "", err
	}
	rows, err := tx.Query("SELECT hash FROM pages WHERE context_id=? ORDER BY position", id)
	if err != nil {
		return nil, "", err
	}
	for rows.Next() {
		var hash string
		if err = rows.Scan(&hash); err != nil {
			rows.Close()
			return nil, "", err
		}
		pages = append(pages, hash)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	var nullable sql.NullString
	if err = tx.QueryRow("SELECT tail FROM contexts WHERE id=?", id).Scan(&nullable); err != nil {
		return nil, "", err
	}
	if nullable.Valid {
		tail = nullable.String
	}
	return pages, tail, nil
}

func snapshotContext(tx *sql.Tx, id string) (ContextSnapshot, error) {
	snap := ContextSnapshot{ContextID: id, Prefix: []ContextPage{}}
	rows, err := tx.Query("SELECT hash FROM pages WHERE context_id=? ORDER BY position", id)
	if err != nil {
		return snap, err
	}
	var hashes []string
	for rows.Next() {
		var hash string
		if err = rows.Scan(&hash); err != nil {
			rows.Close()
			return snap, err
		}
		hashes = append(hashes, hash)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return snap, err
	}
	for _, hash := range hashes {
		data, err := contextBlob(tx, hash)
		if err != nil {
			return snap, err
		}
		snap.Prefix = append(snap.Prefix, ContextPage{hash, int64(len(data))})
	}
	var tail sql.NullString
	if err = tx.QueryRow("SELECT tail FROM contexts WHERE id=?", id).Scan(&tail); err != nil {
		return snap, err
	}
	if tail.Valid {
		data, err := contextBlob(tx, tail.String)
		if err != nil {
			return snap, err
		}
		snap.Tail = &ContextPage{tail.String, int64(len(data))}
	}
	return snap, nil
}

func (s *ContextStore) Snapshot(owner, id string) (ContextSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return ContextSnapshot{}, err
	}
	defer tx.Rollback()
	if err = authorizeContext(tx, owner, id); err != nil {
		return ContextSnapshot{}, err
	}
	return snapshotContext(tx, id)
}

func (s *ContextStore) Checkpoint(owner, id, key string) (string, error) {
	return s.mutate(owner, id, key, "checkpoint", nil, func(tx *sql.Tx) (string, error) {
		snap, err := snapshotContext(tx, id)
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(snap)
		if err != nil {
			return "", err
		}
		hash, err := putContextBlob(tx, data)
		if err != nil {
			return "", err
		}
		_, err = tx.Exec("INSERT OR IGNORE INTO grants VALUES(?,?,?)", id, hash, "checkpoint")
		return hash, err
	})
}

func (s *ContextStore) Restore(owner, id, key, checkpoint string) (string, error) {
	return s.mutate(owner, id, key, "restore", []byte(checkpoint), func(tx *sql.Tx) (string, error) {
		var exists int
		if tx.QueryRow("SELECT 1 FROM grants WHERE context_id=? AND hash=? AND kind='checkpoint'", id, checkpoint).Scan(&exists) != nil {
			return "", ErrContextAccess
		}
		data, err := contextBlob(tx, checkpoint)
		if err != nil {
			return "", err
		}
		var snap ContextSnapshot
		if json.Unmarshal(data, &snap) != nil || snap.ContextID != id {
			return "", ErrContextCorrupt
		}
		if _, err = tx.Exec("DELETE FROM pages WHERE context_id=?", id); err != nil {
			return "", err
		}
		for i, p := range snap.Prefix {
			data, err := contextBlob(tx, p.Hash)
			if err != nil {
				return "", err
			}
			if int64(len(data)) != p.Bytes {
				return "", ErrContextCorrupt
			}
			if _, err = tx.Exec("INSERT INTO pages VALUES(?,?,?)", id, i, p.Hash); err != nil {
				return "", err
			}
		}
		var tail any
		if snap.Tail != nil {
			data, err := contextBlob(tx, snap.Tail.Hash)
			if err != nil {
				return "", err
			}
			if int64(len(data)) != snap.Tail.Bytes {
				return "", ErrContextCorrupt
			}
			tail = snap.Tail.Hash
		}
		_, err = tx.Exec("UPDATE contexts SET tail=? WHERE id=?", tail, id)
		return checkpoint, err
	})
}

func (s *ContextStore) CommitArtifact(owner, id, key string, data []byte) (string, error) {
	return s.mutate(owner, id, key, "artifact", data, func(tx *sql.Tx) (string, error) {
		hash, err := putContextBlob(tx, data)
		if err != nil {
			return "", err
		}
		_, err = tx.Exec("INSERT OR IGNORE INTO grants VALUES(?,?,?)", id, hash, "artifact")
		return hash, err
	})
}

func (s *ContextStore) ReadArtifact(owner, id, handle string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = authorizeContext(tx, owner, id); err != nil {
		return nil, err
	}
	var exists int
	if tx.QueryRow("SELECT 1 FROM grants WHERE context_id=? AND hash=? AND kind='artifact'", id, handle).Scan(&exists) != nil {
		return nil, ErrContextAccess
	}
	return contextBlob(tx, handle)
}

func (s *ContextStore) ReadPage(owner, id, hash string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = authorizeContext(tx, owner, id); err != nil {
		return nil, err
	}
	var exists int
	if tx.QueryRow("SELECT 1 FROM pages WHERE context_id=? AND hash=? UNION SELECT 1 FROM contexts WHERE id=? AND tail=?", id, hash, id, hash).Scan(&exists) != nil {
		return nil, ErrContextAccess
	}
	return contextBlob(tx, hash)
}

func (s *ContextStore) Stats(owner string, ids ...string) (ContextStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return ContextStats{}, err
	}
	defer tx.Rollback()
	stats := ContextStats{}
	refs := map[string]int{}
	sizes := map[string]int64{}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if err = authorizeContext(tx, owner, id); err != nil {
			return ContextStats{}, err
		}
		snap, err := snapshotContext(tx, id)
		if err != nil {
			return ContextStats{}, err
		}
		stats.Contexts++
		pages := snap.Prefix
		if snap.Tail != nil {
			pages = append(pages, *snap.Tail)
		}
		for _, p := range pages {
			refs[p.Hash]++
			sizes[p.Hash] = p.Bytes
			stats.LogicalBytes += p.Bytes
			stats.References++
		}
	}
	stats.UniqueBlobs = len(refs)
	for hash, count := range refs {
		stats.UniqueBytes += sizes[hash]
		if count > 1 {
			stats.SharedBlobs++
		}
	}
	stats.SavedBytes = stats.LogicalBytes - stats.UniqueBytes
	return stats, nil
}

func (s *ContextStore) Commits(owner, id string, after int64, limit int) ([]LSFSCommit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if after < 0 || limit < 1 || limit > 1000 {
		return nil, errors.New("invalid commit cursor or limit")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = authorizeContext(tx, owner, id); err != nil {
		return nil, err
	}
	rows, err := tx.Query("SELECT cursor,context_id,kind,handle,key FROM commits WHERE context_id=? AND cursor>? ORDER BY cursor LIMIT ?", id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []LSFSCommit{}
	for rows.Next() {
		var e LSFSCommit
		if err = rows.Scan(&e.Cursor, &e.ContextID, &e.Kind, &e.Handle, &e.IdempotencyKey); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
