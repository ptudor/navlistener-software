package commissioning

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"

	"github.com/ptudor/navlistener/internal/attestation"
	"github.com/ptudor/navlistener/internal/identity"
)

// ProductPolicy is scoped to one manufacturer and one product/revision pair.
// The recorded RTC is history, not identity, so no policy depends on it.
type ProductPolicy struct {
	Product  uint16 `toml:"product"`
	Revision uint16 `toml:"revision"`
}

func (p ProductPolicy) Allows(s Statement) bool {
	return p.Product == uint16(s.Product) && p.Revision == s.BoardRevision
}

// Authorities selects exactly one verifier from authenticated enrollment data.
// It never discovers a manufacturer by trying other authorities' keys.
type Authorities map[string]*Verifier

func (a Authorities) Evaluate(c identity.ObserverContext, e Evidence, exported []byte) (Result, error) {
	v := a[c.ManufacturerAuthorityID]
	if v == nil {
		return reject(ReasonUnconfigured, errors.New("enrolled manufacturer authority is unknown or disabled"))
	}
	r, err := v.Evaluate(c.ObserverID, e, exported)
	if err != nil {
		return r, err
	}
	if uint16(r.Statement.Product) != c.HardwareProduct || r.Statement.BoardRevision != c.HardwareRevision ||
		hex.EncodeToString(r.Statement.Attestation[:]) != c.CoreAttestationFingerprint {
		return reject(ReasonIdentity, errors.New("commissioning does not match enrolled product, revision and core record"))
	}
	return r, nil
}

func (a Authorities) Recheck(r Result) error {
	if r.Trust == identity.HardwareTrustNone || r.Trust == "" {
		return nil
	}
	v := a[r.ManufacturerAuthorityID]
	if v == nil {
		return errors.New("manufacturer authority no longer enabled")
	}
	return v.Recheck(r)
}

// PublicKeys returns copies for key-role/authority registration and core checks.
func (ks *KeySet) PublicKeys() []*ecdsa.PublicKey {
	var result []*ecdsa.PublicKey
	for _, key := range ks.keys {
		der, _ := x509.MarshalPKIXPublicKey(key)
		copy, _ := x509.ParsePKIXPublicKey(der)
		result = append(result, copy.(*ecdsa.PublicKey))
	}
	return result
}

func KeyFingerprint(key *ecdsa.PublicKey) string {
	der, _ := x509.MarshalPKIXPublicKey(key)
	d := sha256.Sum256(der)
	return hex.EncodeToString(d[:])
}

func (ks *KeySet) SignerFingerprint(id [KeyIDSize]byte) string {
	if key := ks.keys[id]; key != nil {
		return KeyFingerprint(key)
	}
	return ""
}

// VerifyCore checks only this preselected authority's manufacturer keys and
// returns the exact successful SPKI, independently of the commissioning signer.
func (ks *KeySet) VerifyCore(record attestation.Record, h attestation.HardwareIdentity) (attestation.Verification, string, error) {
	for _, key := range ks.keys {
		v, err := attestation.Verify(record, h, key)
		if err == nil {
			return v, KeyFingerprint(key), nil
		}
	}
	return attestation.Verification{}, "", errors.New("core record has no valid signature in the expected manufacturer authority")
}

func (v *Verifier) SetProducts(products []ProductPolicy) error {
	seen := map[[2]uint16]bool{}
	if len(products) == 0 {
		return errors.New("manufacturer requires explicit product/revision policies")
	}
	for _, p := range products {
		id := [2]uint16{p.Product, p.Revision}
		if p.Product == 0 || seen[id] {
			return errors.New("invalid or duplicate product/revision policy")
		}
		seen[id] = true
	}
	v.products = append([]ProductPolicy(nil), products...)
	return nil
}
