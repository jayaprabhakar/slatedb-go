package block

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/slatedb/slatedb-go/internal/assert"
	"github.com/slatedb/slatedb-go/internal/compress"
	"github.com/slatedb/slatedb-go/internal/types"
	"github.com/slatedb/slatedb-go/slatedb/common"
	"hash/crc32"
	"math"
)

var (
	ErrEmptyBlock = errors.New("empty block")
)

const (
	// TODO(thrawn01): Remove once KeyValue refactor is complete
	Tombstone = math.MaxUint32
)

type Block struct {
	FirstKey []byte
	Data     []byte
	Offsets  []uint16
}

// Encode encodes the Block into a byte slice using the following format
// +-----------------------------------------------+
// |               Block                           |
// +-----------------------------------------------+
// |  +-----------------------------------------+  |
// |  |  Block.Data                             |  |
// |  |  (List of KeyValues)                    |  |
// |  |  +-----------------------------------+  |  |
// |  |  | KeyValue Format (See row.go)      |  |  |
// |  |  +-----------------------------------+  |  |
// |  |  ...                                    |  |
// |  +-----------------------------------------+  |
// |  +-----------------------------------------+  |
// |  |  Block.Offsets                          |  |
// |  |  +-----------------------------------+  |  |
// |  |  |  Offset of KeyValue (2 bytes)     |  |  |
// |  |  +-----------------------------------+  |  |
// |  |  ...                                    |  |
// |  +-----------------------------------------+  |
// |  +-----------------------------------------+  |
// |  |  Number of Offsets (2 bytes)            |  |
// |  +-----------------------------------------+  |
// |  |  Checksum (4 bytes)                     |  |
// |  +-----------------------------------------+  |
// +-----------------------------------------------+
func Encode(b *Block, codec compress.Codec) ([]byte, error) {
	bufSize := len(b.Data) + len(b.Offsets)*common.SizeOfUint16 + common.SizeOfUint16

	buf := make([]byte, 0, bufSize)
	buf = append(buf, b.Data...)

	for _, offset := range b.Offsets {
		buf = binary.BigEndian.AppendUint16(buf, offset)
	}
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(b.Offsets)))

	compressed, err := compress.Encode(buf, codec)
	if err != nil {
		return nil, err
	}

	// Make a new buffer exactly the size of the compressed plus the checksum
	buf = make([]byte, 0, len(compressed)+common.SizeOfUint32)
	buf = append(buf, compressed...)
	buf = binary.BigEndian.AppendUint32(buf, crc32.ChecksumIEEE(compressed))
	return buf, nil
}

// Decode converts the encoded byte slice into the provided Block
// TODO(thrawn01): The asserts here should be returned as errors as its impossible
//  to know which block is corrupt without context, nor is it possible to gracefully skip
//  corrupt blocks if these assertions fail.
func Decode(b *Block, input []byte, codec compress.Codec) error {
	assert.True(len(input) > 6, "corrupt block; block is too small; must be at least 6 bytes")

	// last 4 bytes hold the checksum
	checksumIndex := len(input) - common.SizeOfUint32
	compressed := input[:checksumIndex]
	if binary.BigEndian.Uint32(input[checksumIndex:]) != crc32.ChecksumIEEE(compressed) {
		return common.ErrChecksumMismatch
	}

	buf, err := compress.Decode(compressed, codec)
	if err != nil {
		return err
	}

	assert.True(len(buf) > common.SizeOfUint16,
		"corrupt block; uncompressed block is too small; must be at least %d bytes", common.SizeOfUint16)

	// The last 2 bytes hold the offset count
	offsetCountIndex := len(buf) - common.SizeOfUint16
	offsetCount := binary.BigEndian.Uint16(buf[offsetCountIndex:])

	offsetStartIndex := offsetCountIndex - (int(offsetCount) * common.SizeOfUint16)
	assert.True(offsetStartIndex >= 0,
		"corrupt block; offset index %d cannot be negative", offsetStartIndex)
	offsets := make([]uint16, 0, offsetCount)

	for i := 0; i < int(offsetCount); i++ {
		index := offsetStartIndex + (i * common.SizeOfUint16)
		assert.True(index >= 0 && index <= len(buf), "corrupt block; block offset[%d] is invalid", index)
		offsets = append(offsets, binary.BigEndian.Uint16(buf[index:]))
	}

	b.Data = buf[:offsetStartIndex]
	b.Offsets = offsets

	assert.True(len(b.Offsets) != 0, "corrupt block; block Block.Offsets must be greater than 0")

	// Extract the first key in the block
	keyLen := binary.BigEndian.Uint16(b.Data[b.Offsets[0]:])
	b.FirstKey = b.Data[b.Offsets[0]+2 : b.Offsets[0]+2+keyLen]

	return nil
}

type Builder struct {
	offsets   []uint16
	data      []byte
	blockSize uint64
	firstKey  []byte
}

// NewBuilder builds a block of key values in the v0RowCodec
// format along with the Block.Offsets which point to the
// beginning of each key/value.
//
// See v0RowCodec for on disk format of the key values.
func NewBuilder(blockSize uint64) *Builder {
	return &Builder{
		offsets:   make([]uint16, 0),
		data:      make([]byte, 0),
		blockSize: blockSize,
	}
}

func (b *Builder) curBlockSize() int {
	return common.SizeOfUint16 + // number of key-value pairs in the block
		(len(b.offsets) * common.SizeOfUint16) + // offsets
		len(b.data) // Row entries already in the block
}

// TODO(thrawn01): Should accept a types.EntryRow instead of block.v0Row, I think?
func (b *Builder) Add(key []byte, row v0Row) bool {
	assert.True(len(key) > 0, "key must not be empty")
	row.KeyPrefixLen = computePrefix(b.firstKey, key)
	row.KeySuffix = key[row.KeyPrefixLen:]

	// If adding the key-value pair would exceed the block size limit, don't add it.
	// (Unless the block is empty, in which case, allow the block to exceed the limit.)
	if uint64(b.curBlockSize()+row.Size()) > b.blockSize && !b.IsEmpty() {
		return false
	}

	b.offsets = append(b.offsets, uint16(len(b.data)))
	b.data = append(b.data, v0RowCodec.Encode(row)...)

	if b.firstKey == nil {
		b.firstKey = bytes.Clone(key)
	}
	return true
}

func (b *Builder) AddValue(key []byte, value []byte) bool {
	if len(value) == 0 {
		return b.Add(key, v0Row{Value: types.Value{Kind: types.KindTombStone}})
	}
	return b.Add(key, v0Row{Value: types.Value{Value: value}})
}

func (b *Builder) IsEmpty() bool {
	return len(b.offsets) == 0
}

func (b *Builder) Build() (*Block, error) {
	if b.IsEmpty() {
		return nil, ErrEmptyBlock
	}
	return &Block{
		FirstKey: b.firstKey,
		Offsets:  b.offsets,
		Data:     b.data,
	}, nil
}

func PrettyPrint(block *Block) string {
	buf := new(bytes.Buffer)
	it := NewIterator(block)
	for _, offset := range block.Offsets {
		kv, ok := it.NextEntry()
		if !ok {
			_, _ = fmt.Fprintf(buf, "WARN: there are more offsets than blocks")
		}
		_, _ = fmt.Fprintf(buf, "Offset: %d\n", offset)
		_, _ = fmt.Fprintf(buf, "  uint16(%d) - 2 bytes\n", len(kv.Key))
		_, _ = fmt.Fprintf(buf, "  []byte(\"%s\") - %d bytes\n", Truncate(kv.Key, 30), len(kv.Key))
		if kv.Value.IsTombstone() {
			_, _ = fmt.Fprintf(buf, "  uint32(%d) - 4 bytes\n", Tombstone)
		} else {
			v := kv.Value.Value
			_, _ = fmt.Fprintf(buf, "  uint32(%d) - 4 bytes\n", len(v))
			_, _ = fmt.Fprintf(buf, "  []byte(\"%s\") - %d bytes\n", Truncate(v, 30), len(v))
		}
	}
	if _, ok := it.NextEntry(); ok {
		_, _ = fmt.Fprintf(buf, "WARN: there are more blocks than offsets")
	}
	return buf.String()
}

// Truncate takes a given byte slice and truncates it to the provided
// length appending "..." to the end if the slice was truncated and returning
// the result as a string.
func Truncate(data []byte, maxLength int) string {
	if len(data) <= maxLength {
		return string(data)
	}
	maxLength -= 3
	truncated := make([]byte, maxLength)
	copy(truncated, data[:maxLength])
	return fmt.Sprintf("%s...", truncated)
}
