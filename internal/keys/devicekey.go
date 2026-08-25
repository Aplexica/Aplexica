package keys

// DeviceKeyStore is the compatibility X25519 view over the complete device
// identity store. Creating the wrap key also ensures the independent Ed25519
// signing identity exists.
type DeviceKeyStore struct{ identity DeviceIdentityStore }

func NewDeviceKeyStore(s AtomicSecretsStore) *DeviceKeyStore {
	return &DeviceKeyStore{identity: DeviceIdentityStore{Secrets: s}}
}
func (s *DeviceKeyStore) LoadOrCreate() (priv [X25519KeySize]byte, pub [X25519KeySize]byte, err error) {
	id, err := s.identity.LoadOrCreate()
	if err != nil {
		return priv, pub, err
	}
	return id.WrapPrivate, id.WrapPublic, nil
}
