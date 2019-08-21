package bitcoin

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
)

const Hash32Size = 32

// Hash32 is a 32 byte integer in little endian format.
type Hash32 [Hash32Size]byte

func NewHash32(b []byte) (*Hash32, error) {
	if len(b) != Hash32Size {
		return nil, errors.New("Wrong byte length")
	}
	result := Hash32{}
	copy(result[:], b)
	return &result, nil
}

// NewHash32FromStr creates a little endian hash from a big endian string.
func NewHash32FromStr(s string) (*Hash32, error) {
	if len(s) != 2*Hash32Size {
		return nil, fmt.Errorf("Wrong size hex for Hash32 : %d", len(s))
	}

	b := make([]byte, Hash32Size)
	_, err := hex.Decode(b, []byte(s[:]))
	if err != nil {
		return nil, err
	}

	result := Hash32{}
	reverse32(result[:], b)
	return &result, nil
}

// Bytes returns the data for the hash.
func (h *Hash32) Bytes() []byte {
	return h[:]
}

// SetBytes sets the value of the hash.
func (h *Hash32) SetBytes(b []byte) error {
	if len(b) != Hash32Size {
		return errors.New("Wrong byte length")
	}
	copy(h[:], b)
	return nil
}

// String returns the hex for the hash.
func (h *Hash32) String() string {
	return fmt.Sprintf("%x", h[:])
}

// Equal returns true if the parameter has the same value.
func (h *Hash32) Equal(o *Hash32) bool {
	return bytes.Equal(h[:], o[:])
}

// Serialize writes the hash into a buffer.
func (h *Hash32) Serialize(buf *bytes.Buffer) error {
	_, err := buf.Write(h[:])
	return err
}

// Deserialize reads a hash from a buffer.
func DeserializeHash32(buf *bytes.Reader) (*Hash32, error) {
	result := Hash32{}
	_, err := buf.Read(result[:])
	if err != nil {
		return nil, err
	}

	return &result, err
}

// MarshalJSON converts to json.
func (h *Hash32) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("\"%x\"", h[:])), nil
}

// UnmarshalJSON converts from json.
func (h *Hash32) UnmarshalJSON(data []byte) error {
	if len(data) != (2*Hash32Size)+2 {
		return fmt.Errorf("Wrong size hex for Hash32 : %d", len(data)-2)
	}

	b := make([]byte, Hash32Size)
	_, err := hex.Decode(b, data[1:len(data)-1])
	if err != nil {
		return err
	}
	reverse32(h[:], b)
	return nil
}

func reverse32(h, rh []byte) {
	i := Hash32Size - 1
	for _, b := range rh[:] {
		h[i] = b
		i--
	}
}
