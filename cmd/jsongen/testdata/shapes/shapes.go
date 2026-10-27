// Package shapes holds types jsongen must refuse or accept, for its tests.
package shapes

type Octet uint8

type Blob []Octet

type ReadBytes struct{ B []byte }

type ReadOctets struct{ B []Octet }

type ReadBlob struct{ B Blob }

type ReadOctetArray struct{ B [4]Octet }

// AppendOnly has AppendText and no MarshalText.
type AppendOnly int

func (a AppendOnly) AppendText(b []byte) ([]byte, error) { return append(b, 'a'), nil }

type WriteAppendOnly struct{ A AppendOnly }

// AppendOnlyByte is a byte with AppendText and no MarshalText, whose slices
// are not base64.
type AppendOnlyByte uint8

func (a AppendOnlyByte) AppendText(b []byte) ([]byte, error) { return append(b, 'a'), nil }

type WriteAppendOnlyBytes struct{ B []AppendOnlyByte }

// MarshalOnly has MarshalText and no AppendText.
type MarshalOnly int

func (m MarshalOnly) MarshalText() ([]byte, error) { return []byte("m"), nil }

type WriteMarshalOnly struct{ M MarshalOnly }

type WriteOctets struct {
	B Blob
	C []Octet
}
