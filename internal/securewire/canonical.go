package securewire

import "github.com/fxamacker/cbor/v2"

var enc cbor.EncMode
var dec cbor.DecMode

func init() {
	var err error
	enc, err = cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	dec, err = cbor.DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF, IndefLength: cbor.IndefLengthForbidden, TagsMd: cbor.TagsForbidden, MaxNestedLevels: 32, MaxArrayElements: 2048, MaxMapPairs: 2048, ExtraReturnErrors: cbor.ExtraDecErrorUnknownField}.DecMode()
	if err != nil {
		panic(err)
	}
}
func Canonical(values ...any) ([]byte, error) { return enc.Marshal(values) }
func Decode(data []byte, v any) error         { return dec.Unmarshal(data, v) }
