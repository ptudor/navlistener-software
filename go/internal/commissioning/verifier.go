package commissioning

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
)

// Rejection reasons are stable strings: they label metrics and appear in logs.
const (
	// ReasonUnconfigured is reported by a collector that pins no manufacturer
	// keys: it reads evidence to keep the handshake in step and proves nothing.
	ReasonUnconfigured = "unconfigured"
	ReasonMalformed    = "malformed"
	ReasonSignature    = "signature"
	ReasonProduct      = "product"
	ReasonIdentity     = "identity"
	ReasonUnlisted     = "unlisted"
	ReasonRevoked      = "revoked"
	ReasonSuperseded   = "superseded"
	ReasonProofMissing = "proof_missing"
	ReasonProof        = "proof"
)

// Rejection explains why presented evidence established no hardware trust.
// A rejection never ends a session by itself: evidence labels data, and the
// operational credential decides admission.
type Rejection struct {
	Reason string
	Err    error
}

func (r *Rejection) Error() string {
	return "commissioning evidence rejected (" + r.Reason + "): " + r.Err.Error()
}
func (r *Rejection) Unwrap() error { return r.Err }

func reject(reason string, err error) (Result, error) {
	return Result{Trust: identity.HardwareTrustNone}, &Rejection{Reason: reason, Err: err}
}

// Result is what verified evidence established for one session.
type Result struct {
	ManufacturerAuthorityID string
	CommissioningSignerSPKI string
	RegistrySignerSPKI      string
	Trust                   identity.HardwareTrust
	Statement               Statement
	Fingerprint             [32]byte
}

// Verifier evaluates device evidence against the pinned manufacturer keys and,
// when configured, the current signed registry.
type Verifier struct {
	manufacturerAuthorityID string
	manufacturer            *KeySet
	products                []ProductPolicy
	registryKeys            *KeySet
	requireEntry            bool
	registry                atomic.Pointer[RegistryIndex]
	// floor is the newest registry sequence adopted by an earlier process, read
	// from statePath. Nothing older is ever loaded, even as the first registry.
	floor     atomic.Uint64
	statePath string
	recorded  uint64 // last sequence written to statePath; owned by WatchRegistry
}

// NewVerifier pins the manufacturer keys. Without a registry every valid
// record is honoured and nothing can be withdrawn.
func NewVerifier(manufacturerAuthorityID string, manufacturer *KeySet) (*Verifier, error) {
	if !identity.ValidScopeID(manufacturerAuthorityID) {
		return nil, errors.New("manufacturer authority id is required and must be a valid scope id")
	}
	if manufacturer == nil {
		return nil, errors.New("manufacturer keys are required")
	}
	return &Verifier{manufacturerAuthorityID: manufacturerAuthorityID, manufacturer: manufacturer}, nil
}

// UseRegistry pins the registry keys. requireEntry additionally withholds
// trust from a board the loaded registry does not list; leave it false where a
// registry copy may lag behind newly commissioned boards.
func (v *Verifier) UseRegistry(keys *KeySet, requireEntry bool) error {
	if keys == nil {
		return errors.New("registry keys are required")
	}
	v.registryKeys, v.requireEntry = keys, requireEntry
	return nil
}

// SetRegistryFloor refuses any registry older than sequence, including the first
// one loaded. It only ever rises. A configuration check uses it with
// ReadRegistryState to predict what the daemon will accept without writing.
func (v *Verifier) SetRegistryFloor(sequence uint64) {
	for {
		current := v.floor.Load()
		if sequence <= current || v.floor.CompareAndSwap(current, sequence) {
			return
		}
	}
}

// UseRegistryState makes the registry sequence survive a restart: the recorded
// sequence becomes the floor now, and WatchRegistry records each registry it
// adopts. Call before WatchRegistry.
func (v *Verifier) UseRegistryState(path string) error {
	if path == "" {
		return errors.New("registry state path is required")
	}
	floor, err := ReadRegistryState(path, v.manufacturerAuthorityID)
	if err != nil {
		return err
	}
	v.SetRegistryFloor(floor)
	v.statePath, v.recorded = path, floor
	return nil
}

// LoadRegistry verifies and adopts a registry. A registry older than the one
// already loaded, or than the recorded floor, is refused, so a stale copy
// cannot restore a withdrawn board.
func (v *Verifier) LoadRegistry(data []byte) (*RegistryIndex, error) {
	if v.registryKeys == nil {
		return nil, errors.New("no registry keys are pinned")
	}
	ix, err := VerifyRegistry(data, v.registryKeys, v.manufacturerAuthorityID)
	if err != nil {
		return nil, err
	}
	if floor := v.floor.Load(); ix.Sequence < floor {
		return nil, fmt.Errorf("registry sequence %d is older than sequence %d, which this collector has already adopted", ix.Sequence, floor)
	}
	for {
		current := v.registry.Load()
		if current != nil && ix.Sequence < current.Sequence {
			return nil, fmt.Errorf("registry sequence %d is older than the loaded sequence %d", ix.Sequence, current.Sequence)
		}
		if v.registry.CompareAndSwap(current, ix) {
			return ix, nil
		}
	}
}

// Registry returns the loaded registry, or nil.
func (v *Verifier) Registry() *RegistryIndex { return v.registry.Load() }

// WatchRegistry loads path and reloads it whenever the file changes, until ctx
// ends. The first load is synchronous and its failure is returned, so a
// collector configured with a registry never starts without one. A later
// reload that fails keeps the registry already in force and is logged.
//
// With UseRegistryState, each adopted sequence is recorded. At startup a state
// file that cannot be written is a failure like any other. On a later reload
// the registry is adopted regardless — it may carry a withdrawal, and refusing
// it to protect the floor would leave that withdrawal unapplied — and the
// failure is logged and passed to loaded, so it is not silent.
func (v *Verifier) WatchRegistry(ctx context.Context, path string, every time.Duration, log *slog.Logger, loaded func(*RegistryIndex, error)) error {
	if every <= 0 {
		return errors.New("registry reload interval must be positive")
	}
	if log == nil {
		log = slog.Default()
	}
	seen, err := v.loadRegistryFile(path)
	if err == nil {
		err = v.recordRegistrySequence()
	}
	if loaded != nil {
		loaded(v.Registry(), err)
	}
	if err != nil {
		return err
	}
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			st, err := os.Stat(path)
			if err == nil && st.ModTime().Equal(seen.modTime) && st.Size() == seen.size {
				continue
			}
			next, err := v.loadRegistryFile(path)
			if loaded != nil {
				loaded(v.Registry(), err)
			}
			if err != nil {
				log.Error("registry reload failed; keeping the registry already in force", "path", path, "error", err)
				continue
			}
			seen = next
			log.Info("registry reloaded", "path", path, "sequence", v.Registry().Sequence, "boards", v.Registry().Len())
			if err := v.recordRegistrySequence(); err != nil {
				log.Error("registry adopted, but its sequence was not recorded; a restart could accept an older registry", "path", path, "error", err)
				if loaded != nil {
					loaded(v.Registry(), err)
				}
			}
		}
	}()
	return nil
}

// recordRegistrySequence raises the floor to the registry in force and, with a
// state path, records it for the next process.
func (v *Verifier) recordRegistrySequence() error {
	ix := v.registry.Load()
	if ix == nil {
		return nil
	}
	v.SetRegistryFloor(ix.Sequence)
	if v.statePath == "" || ix.Sequence == v.recorded {
		return nil
	}
	if err := writeRegistryState(v.statePath, v.manufacturerAuthorityID, ix.Sequence); err != nil {
		return err
	}
	v.recorded = ix.Sequence
	return nil
}

type fileStamp struct {
	modTime time.Time
	size    int64
}

func (v *Verifier) loadRegistryFile(path string) (fileStamp, error) {
	st, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, fmt.Errorf("registry: %w", err)
	}
	if st.Size() > registryMaxBytes {
		return fileStamp{}, fmt.Errorf("registry %s is %d bytes, limit %d", path, st.Size(), registryMaxBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fileStamp{}, fmt.Errorf("registry: %w", err)
	}
	if _, err := v.LoadRegistry(data); err != nil {
		return fileStamp{}, fmt.Errorf("registry %s: %w", path, err)
	}
	return fileStamp{modTime: st.ModTime(), size: st.Size()}, nil
}

// Evaluate decides what evidence proves for the authenticated observer on one
// TLS session. exported is that session's keying material for ExporterLabel.
//
// Trusted needs all of: a record signed by a pinned manufacturer key, naming
// this observer, current and not revoked in the registry, and a session proof
// from the microcontroller key the record names. Open and test records prove
// only what they say and need no proof.
func (v *Verifier) Evaluate(observerID string, e Evidence, exported []byte) (Result, error) {
	s, err := v.manufacturer.Verify(e.Record)
	if err != nil {
		return reject(ReasonSignature, err)
	}
	// The manufacturer key signs for other product lines too. Only a board built
	// as an observer proves anything to an observer collector.
	allowed := false
	for _, policy := range v.products {
		allowed = allowed || policy.Allows(s)
	}
	if !allowed {
		return reject(ReasonProduct, fmt.Errorf("record is for product %d, not an observer", s.Product))
	}
	if s.ObserverID() != observerID {
		return reject(ReasonIdentity, fmt.Errorf("record is for observer %s, session is %s", s.ObserverID(), observerID))
	}
	fp := e.Record.Fingerprint()
	registry := v.registry.Load()
	if rejection := v.checkRegistryIndex(s, fp, registry); rejection != nil {
		return Result{Trust: identity.HardwareTrustNone}, rejection
	}
	if len(e.MCUKey) != 0 {
		if err := VerifyProof(s, e.Record, e.MCUKey, exported, e.Proof); err != nil {
			return reject(ReasonProof, err)
		}
	} else if s.Profile == ProfileTrusted {
		return reject(ReasonProofMissing, errors.New("trusted record presented without a session proof"))
	}
	result := Result{ManufacturerAuthorityID: v.manufacturerAuthorityID, Statement: s, Fingerprint: fp}
	result.CommissioningSignerSPKI = v.manufacturer.SignerFingerprint(e.Record.KeyID())
	if registry != nil {
		result.RegistrySignerSPKI = registry.SignerSPKI
	}
	switch s.Profile {
	case ProfileTrusted:
		result.Trust = identity.HardwareTrustTrusted
	case ProfileOpen:
		result.Trust = identity.HardwareTrustOpen
	default:
		result.Trust = identity.HardwareTrustTest
	}
	return result, nil
}

// checkRegistry applies the registry's three subtractive checks to a record
// that has already verified.
func (v *Verifier) checkRegistry(s Statement, fp [32]byte) *Rejection {
	return v.checkRegistryIndex(s, fp, v.registry.Load())
}

func (v *Verifier) checkRegistryIndex(s Statement, fp [32]byte, ix *RegistryIndex) *Rejection {
	if ix == nil {
		return nil
	}
	board, listed := ix.boards[s.BoardUID]
	switch {
	case !listed && v.requireEntry:
		return &Rejection{Reason: ReasonUnlisted, Err: errors.New("board is not in the registry")}
	case listed && board.status == StatusRevoked:
		return &Rejection{Reason: ReasonRevoked, Err: fmt.Errorf("board is revoked: %s", board.reason)}
	case listed && board.fingerprint != fp:
		return &Rejection{Reason: ReasonSuperseded, Err: errors.New("record is not the board's current commissioning record")}
	}
	return nil
}

// Recheck applies the registry checks again to a result Evaluate accepted
// earlier, so a long-lived session notices a registry that has since withdrawn
// or superseded its board. It can only take trust away.
func (v *Verifier) Recheck(r Result) error {
	if r.Trust == identity.HardwareTrustNone {
		return nil
	}
	if r.ManufacturerAuthorityID != v.manufacturerAuthorityID {
		return errors.New("result belongs to another manufacturer authority")
	}
	if rejection := v.checkRegistry(r.Statement, r.Fingerprint); rejection != nil {
		return rejection
	}
	return nil
}

// LoadKeySet reads P-256 public keys, or certificates carrying them, from PEM
// files and pins them.
func LoadKeySet(paths []string) (*KeySet, error) {
	keys := make([]*ecdsa.PublicKey, 0, len(paths))
	for _, path := range paths {
		key, err := readPublicKey(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		keys = append(keys, key)
	}
	return NewKeySet(keys...)
}

func readPublicKey(path string) (*ecdsa.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, rest := pem.Decode(b)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("no PEM block")
	}
	var parsed any
	if block.Type != "PUBLIC KEY" {
		return nil, errors.New("manufacturer and registry pins require a public key, not a CA certificate")
	}
	if parsed, err = x509.ParsePKIXPublicKey(block.Bytes); err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("key type %T: want ECDSA P-256", parsed)
	}
	return key, nil
}
