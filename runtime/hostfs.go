package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The hostfs bridge (docs/abi/hostfs-model.md): mounts grant an
// application scoped filesystem access under a guest path. The path
// string grants nothing by itself — authority is this persistent,
// capability-narrowed mount record. lsfs-schema.md hostfs_mounts in
// runtime-snapshot form.
type HostfsMount struct {
	MountID       string `json:"mount_id"`
	ApplicationID string `json:"application_id"`
	HostPath      string `json:"host_path"`  // information label
	GuestPath     string `json:"guest_path"` // contract label; no authority
	Mode          string `json:"mode"`       // ro | rw | append-only | dropbox-in | dropbox-out
	LimitsBytes   int64  `json:"limits_bytes,omitempty"`
	Version       uint64 `json:"version"`
	Enabled       bool   `json:"enabled"`
	CreatedAt     int64  `json:"created_at"`
	ExpiresAt     int64  `json:"expires_at,omitempty"`
}

var (
	ErrMountNotFound = errors.New("hostfs mount not found")
	ErrMountDenied   = errors.New("hostfs mount denied")
)

var hostfsModes = map[string]bool{"ro": true, "rw": true, "append-only": true, "dropbox-in": true, "dropbox-out": true}

// MountHostfs creates a persistent, versioned mount grant. The sandbox
// launcher consumes the same mode vocabulary at exec time; this record is
// the durable authority.
func (s *Supervisor) MountHostfs(principal, applicationID, guestPath, mode string, limits int64, ttl time.Duration) (HostfsMount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Applications[applicationID]; !ok {
		return HostfsMount{}, ErrApplicationNotFound
	}
	if !hostfsModes[mode] || guestPath == "" || limits < 0 {
		return HostfsMount{}, errors.New("invalid hostfs mount")
	}
	// Mount paths are stored canonical: a trailing slash or double slash
	// would make the mount root unreachable at check time.
	if filepath.Clean(guestPath) != guestPath || !filepath.IsAbs(guestPath) {
		return HostfsMount{}, errors.New("invalid hostfs mount path")
	}
	s.state.NextSequence++
	mount := HostfsMount{
		MountID:       fmt.Sprintf("mnt-%d", s.state.NextSequence),
		ApplicationID: applicationID,
		HostPath:      "supervisor://hostfs/" + mode,
		GuestPath:     guestPath,
		Mode:          mode,
		LimitsBytes:   limits,
		Version:       1,
		Enabled:       true,
		CreatedAt:     time.Now().UnixNano(),
	}
	if ttl != 0 {
		mount.ExpiresAt = time.Now().Add(ttl).UnixNano()
	}
	s.state.HostfsMounts[mount.MountID] = mount
	_ = s.auditLocked(principal, applicationID, "hostfs.mount", mount.MountID, "allow", mode)
	return mount, s.persistLocked(nil)
}

// UnmountHostfs revokes a mount; in-flight exec holders are torn down by
// the sandbox launcher at the next exec boundary.
func (s *Supervisor) UnmountHostfs(principal, mountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	mount, ok := s.state.HostfsMounts[mountID]
	if !ok {
		return ErrMountNotFound
	}
	mount.Enabled = false
	mount.Version++
	s.state.HostfsMounts[mountID] = mount
	_ = s.auditLocked(principal, mount.ApplicationID, "hostfs.unmount", mountID, "allow", "")
	return s.persistLocked(nil)
}

// CheckHostfs reports whether a path access is currently authorized for an
// application under the mount's mode. Guest paths are matched by prefix;
// the mode gates the operation class.
func (s *Supervisor) CheckHostfs(applicationID, guestPath, operation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Traversal must die at the gate: normalize before matching so ../
	// sequences cannot walk out of a granted prefix.
	if filepath.Clean(guestPath) != guestPath || !filepath.IsAbs(guestPath) ||
		strings.Contains(guestPath, "\x00") {
		return ErrMountDenied
	}
	for _, mount := range s.state.HostfsMounts {
		if mount.ApplicationID != applicationID || !mount.Enabled {
			continue
		}
		if mount.ExpiresAt != 0 && time.Now().UnixNano() > mount.ExpiresAt {
			continue
		}
		if guestPath != mount.GuestPath && !strings.HasPrefix(guestPath, strings.TrimSuffix(mount.GuestPath, "/")+"/") {
			continue
		}
		switch operation {
		case "read":
			if mount.Mode == "ro" || mount.Mode == "rw" {
				return nil
			}
		case "write", "append":
			if mount.Mode == "rw" && operation == "write" {
				return nil
			}
			if mount.Mode == "append-only" && operation == "append" {
				return nil
			}
		case "intake":
			if mount.Mode == "dropbox-in" {
				return nil
			}
		case "emit":
			if mount.Mode == "dropbox-out" {
				return nil
			}
		}
	}
	return ErrMountDenied
}

func (s *Supervisor) ListHostfs(applicationID string) []HostfsMount {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []HostfsMount{}
	for _, mount := range s.state.HostfsMounts {
		if applicationID == "" || mount.ApplicationID == applicationID {
			out = append(out, mount)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// --- Artifact transfer (lsfs-schema artifact_transfers, runtime form) ---

type ArtifactTransfer struct {
	TransferID     string          `json:"transfer_id"`
	ApplicationID  string          `json:"application_id"`
	ArtifactSHA256 string          `json:"artifact_sha256"`
	Direction      string          `json:"direction"` // import | export
	Status         string          `json:"status"`    // pending | committed | failed | expired
	IdempotencyKey string          `json:"idempotency_key"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	CreatedAt      int64           `json:"created_at"`
}

var ErrTransferDenied = errors.New("artifact transfer denied")

// dropOldestTransfersLocked bounds the transfer history; committed entries
// at the head are the oldest, so both directions share one policy.
func (s *Supervisor) dropOldestTransfersLocked(cap int) {
	if len(s.state.Transfers) <= cap {
		return
	}
	s.state.Transfers = append([]ArtifactTransfer(nil),
		s.state.Transfers[len(s.state.Transfers)-cap:]...)
}

const maxTransfers = 256

// ExportArtifact stages an application payload as a durable outbound
// transfer. Exports pass through the taint gate: unmasked taint denies
// (or escalates), so nothing leaves the boundary unexamined.
func (s *Supervisor) ExportArtifact(principal, applicationID, key string, payload json.RawMessage) (ArtifactTransfer, error) {
	if _, err := s.exportApplication(principal, applicationID, string(payload), false, false, ""); err != nil {
		return ArtifactTransfer{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Applications[applicationID]; !ok {
		return ArtifactTransfer{}, ErrApplicationNotFound
	}
	if key == "" || len(key) > 256 || !json.Valid(payload) || len(payload) > maxTextBytes {
		return ArtifactTransfer{}, errors.New("invalid artifact transfer")
	}
	for i := range s.state.Transfers {
		if s.state.Transfers[i].IdempotencyKey == key &&
			s.state.Transfers[i].ApplicationID == applicationID {
			return s.state.Transfers[i], nil
		}
	}
	s.state.NextSequence++
	transfer := ArtifactTransfer{
		TransferID:     fmt.Sprintf("xfer-%d", s.state.NextSequence),
		ApplicationID:  applicationID,
		Direction:      "export",
		Status:         "committed",
		IdempotencyKey: key,
		Payload:        append(json.RawMessage(nil), payload...),
		CreatedAt:      time.Now().UnixNano(),
	}
	s.state.Transfers = append(s.state.Transfers, transfer)
	s.dropOldestTransfersLocked(maxTransfers)
	_ = s.auditLocked(principal, applicationID, "artifact.transfer.export",
		transfer.TransferID, "allow", key)
	return transfer, s.persistLocked(nil)
}

// CommitImport finalizes an inbound transfer; a duplicate idempotency key
// returns the original transfer without re-staging.
func (s *Supervisor) CommitImport(principal, applicationID, key string, payload json.RawMessage) (ArtifactTransfer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Applications[applicationID]; !ok {
		return ArtifactTransfer{}, ErrApplicationNotFound
	}
	if key == "" || len(key) > 256 || !json.Valid(payload) || len(payload) > maxTextBytes {
		return ArtifactTransfer{}, errors.New("invalid artifact transfer")
	}
	for i := range s.state.Transfers {
		if s.state.Transfers[i].IdempotencyKey == key &&
			s.state.Transfers[i].ApplicationID == applicationID {
			return s.state.Transfers[i], nil
		}
	}
	s.state.NextSequence++
	transfer := ArtifactTransfer{
		TransferID:     fmt.Sprintf("xfer-%d", s.state.NextSequence),
		ApplicationID:  applicationID,
		Direction:      "import",
		Status:         "committed",
		IdempotencyKey: key,
		Payload:        append(json.RawMessage(nil), payload...),
		CreatedAt:      time.Now().UnixNano(),
	}
	// Host-imported data is externally tainted by contract (hostfs-model).
	s.accumulateTaint(applicationID, TaintExternal)
	s.state.Transfers = append(s.state.Transfers, transfer)
	s.dropOldestTransfersLocked(maxTransfers)
	_ = s.auditLocked(principal, applicationID, "artifact.transfer.import",
		transfer.TransferID, "allow", key)
	return transfer, s.persistLocked(nil)
}
