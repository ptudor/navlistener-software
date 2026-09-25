package commissioning

// Tests use the explicit synthetic fixture product, never a production default.
func newTestVerifier(id string, keys *KeySet) (*Verifier, error) {
	v, err := NewVerifier(id, keys)
	if err != nil {
		return nil, err
	}
	err = v.SetProducts([]ProductPolicy{{Product: 1, Revision: 258}})
	return v, err
}
