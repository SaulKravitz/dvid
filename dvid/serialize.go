/*
	This file supports serialization/deserialization and compression of data.
*/

package dvid

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	_ "log"

	lz4 "github.com/janelia-flyem/go/golz4"
	"github.com/janelia-flyem/go/snappy-go/snappy"
)

// Compression is the format of compression for storing data.
// NOTE: Should be no more than 8 (3 bits) compression types.
type Compression struct {
	format CompressionFormat
	level  CompressionLevel
}

func (c Compression) Format() CompressionFormat {
	return c.format
}

func (c Compression) Level() CompressionLevel {
	return c.level
}

// MarshalJSON implements the json.Marshaler interface.
func (c Compression) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf(`{"Format":%d,"Level":%d}`, c.format, c.level)), nil
}

// UnmarshalJSON implements the json.Unmarshaler interface.
func (c *Compression) UnmarshalJSON(b []byte) error {
	var m struct {
		Format CompressionFormat
		Level  CompressionLevel
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	c.format = m.Format
	c.level = m.Level
	return nil
}

// MarshalBinary fulfills the encoding.BinaryMarshaler interface.
func (c Compression) MarshalBinary() ([]byte, error) {
	return []byte{byte(c.format), byte(c.level)}, nil
}

// UnmarshalBinary fulfills the encoding.BinaryUnmarshaler interface.
func (c *Compression) UnmarshalBinary(data []byte) error {
	if len(data) != 2 {
		return fmt.Errorf("Cannot unmarshal %d bytes into Compression", len(data))
	}
	c.format = CompressionFormat(data[0])
	c.level = CompressionLevel(data[1])
	return nil
}

func (c Compression) String() string {
	return fmt.Sprintf("%s, level %d", c.format, c.level)
}

// NewCompression returns a Compression struct that maps compression-specific details
// to a DVID-wide compression.
func NewCompression(format CompressionFormat, level CompressionLevel) (Compression, error) {
	if level == NoCompression {
		format = Uncompressed
	}
	switch format {
	case Uncompressed:
		return Compression{format, DefaultCompression}, nil
	case Snappy:
		return Compression{format, DefaultCompression}, nil
	case LZ4:
		return Compression{format, DefaultCompression}, nil
	case Gzip:
		if level != DefaultCompression && (level < 1 || level > 9) {
			return Compression{}, fmt.Errorf("Gzip compression level must be between 1 and 9")
		}
		return Compression{format, level}, nil
	default:
		return Compression{}, fmt.Errorf("Unrecognized compression format requested: %d", format)
	}
}

// CompressionLevel goes from 1 (fastest) to 9 (highest compression)
// as in deflate.  Default compression is -1 so need signed int8.
type CompressionLevel int8

const (
	NoCompression      = 0
	BestSpeed          = 1
	BestCompression    = 9
	DefaultCompression = -1
)

// CompressionFormat specifies the compression algorithm.
type CompressionFormat uint8

const (
	Uncompressed CompressionFormat = 0
	Snappy                         = 1 << (iota - 1)
	Gzip                           // Gzip stores length and checksum automatically.
	LZ4
)

func (format CompressionFormat) String() string {
	switch format {
	case Uncompressed:
		return "No compression"
	case Snappy:
		return "Go Snappy compression"
	case LZ4:
		return "LZ4 compression"
	case Gzip:
		return "gzip compression"
	default:
		return "Unknown compression"
	}
}

// Checksum is the type of checksum employed for error checking stored data.
// NOTE: Should be no more than 4 (2 bits) of checksum types.
type Checksum uint8

const (
	NoChecksum Checksum = 0
	CRC32               = 1 << (iota - 1)
)

// DefaultChecksum is the type of checksum employed for all data operations.
// Note that many database engines already implement some form of corruption test
// and checksum can be set on each datatype instance.
var DefaultChecksum Checksum = NoChecksum

func (checksum Checksum) String() string {
	switch checksum {
	case NoChecksum:
		return "No checksum"
	case CRC32:
		return "CRC32 checksum"
	default:
		return "Unknown checksum"
	}
}

// SerializationFormat combines both compression and checksum methods.
type SerializationFormat uint8

func EncodeSerializationFormat(compress Compression, checksum Checksum) SerializationFormat {
	a := uint8(compress.format&0x07) << 5
	b := uint8(checksum&0x03) << 3
	return SerializationFormat(a | b)
}

func DecodeSerializationFormat(s SerializationFormat) (CompressionFormat, Checksum) {
	format := CompressionFormat(s >> 5)
	checksum := Checksum(s>>3) & 0x03
	return format, checksum
}

// Serialize a slice of bytes using optional compression, checksum.
// Checksum will be ignored if the underlying compression already employs
// checksums, e.g., Gzip.
func SerializeData(data []byte, compress Compression, checksum Checksum) ([]byte, error) {
	var buffer bytes.Buffer

	// Don't duplicate checksum if using Gzip, which already has checksum & length checks.
	if compress.format == Gzip {
		checksum = NoChecksum
	}

	// Store the requested compression and checksum
	format := EncodeSerializationFormat(compress, checksum)
	if err := binary.Write(&buffer, binary.LittleEndian, format); err != nil {
		return nil, err
	}

	// Handle compression if requested
	var err error
	var byteData []byte
	switch compress.format {
	case Uncompressed:
		byteData = data
	case Snappy:
		byteData, err = snappy.Encode(nil, data)
		if err != nil {
			return nil, err
		}
	case LZ4:
		origSize := uint32(len(data))
		byteData = make([]byte, lz4.CompressBound(data)+4)
		binary.LittleEndian.PutUint32(byteData[0:4], origSize)
		var outSize int
		outSize, err = lz4.Compress(data, byteData[4:])
		if err != nil {
			return nil, err
		}
		byteData = byteData[:4+outSize]
	case Gzip:
		var b bytes.Buffer
		w, err := gzip.NewWriterLevel(&b, int(compress.level))
		if err != nil {
			return nil, err
		}
		if _, err = w.Write(data); err != nil {
			return nil, err
		}
		if err = w.Close(); err != nil {
			return nil, err
		}
		byteData = b.Bytes()
	default:
		return nil, fmt.Errorf("Illegal compression (%s) during serialization", compress)
	}

	// Handle checksum if requested
	switch checksum {
	case NoChecksum:
	case CRC32:
		crcChecksum := crc32.ChecksumIEEE(byteData)
		if err := binary.Write(&buffer, binary.LittleEndian, crcChecksum); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("Illegal checksum (%s) in serialize.SerializeData()", checksum)
	}

	// Note the actual data is written last, after any checksum so we don't have to
	// worry about length when deserializing.
	if _, err := buffer.Write(byteData); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// Serializes an arbitrary Go object using Gob encoding and optional compression, checksum.
// If your object is []byte, you should preferentially use SerializeData since the Gob encoding
// process adds some overhead in performance as well as size of wire format to describe the
// transmitted types.
func Serialize(object interface{}, compress Compression, checksum Checksum) ([]byte, error) {
	var buffer bytes.Buffer
	enc := gob.NewEncoder(&buffer)
	err := enc.Encode(object)
	if err != nil {
		return nil, err
	}
	return SerializeData(buffer.Bytes(), compress, checksum)
}

// DeserializeData deserializes a slice of bytes using stored compression, checksum.
// If uncompress parameter is false, the data is not uncompressed.
func DeserializeData(s []byte, uncompress bool) ([]byte, CompressionFormat, error) {
	buffer := bytes.NewBuffer(s)

	// Get the stored compression and checksum
	var format SerializationFormat
	if err := binary.Read(buffer, binary.LittleEndian, &format); err != nil {
		return nil, 0, fmt.Errorf("Could not read serialization format info: %s", err.Error())
	}
	compression, checksum := DecodeSerializationFormat(format)

	// Get any checksum.
	var storedCrc32 uint32
	switch checksum {
	case NoChecksum:
	case CRC32:
		if err := binary.Read(buffer, binary.LittleEndian, &storedCrc32); err != nil {
			return nil, 0, fmt.Errorf("Error reading checksum: %s", err.Error())
		}
	default:
		return nil, 0, fmt.Errorf("Illegal checksum in deserializing data")
	}

	// Get the possibly compressed data.
	cdata := buffer.Bytes()

	// Perform any requested checksum
	switch checksum {
	case CRC32:
		crcChecksum := crc32.ChecksumIEEE(cdata)
		if crcChecksum != storedCrc32 {
			return nil, 0, fmt.Errorf("Bad checksum.  Stored %x got %x", storedCrc32, crcChecksum)
		}
	}

	// Return data with optional compression
	if !uncompress || compression == Uncompressed {
		return cdata, compression, nil
	} else {
		switch compression {
		case Snappy:
			if data, err := snappy.Decode(nil, cdata); err != nil {
				return nil, 0, err
			} else {
				return data, compression, nil
			}
		case LZ4:
			origSize := binary.LittleEndian.Uint32(cdata[0:4])
			data := make([]byte, int(origSize))
			if err := lz4.Uncompress(cdata[4:], data); err != nil {
				return nil, 0, err
			} else {
				return data, compression, nil
			}
		case Gzip:
			b := bytes.NewBuffer(cdata)
			var err error
			r, err := gzip.NewReader(b)
			if err != nil {
				return nil, 0, err
			}
			var buffer bytes.Buffer
			_, err = io.Copy(&buffer, r)
			if err != nil {
				return nil, 0, err
			}
			err = r.Close()
			if err != nil {
				return nil, 0, err
			}
			return buffer.Bytes(), compression, nil
		default:
			return nil, 0, fmt.Errorf("Illegal compression format (%d) in deserialization", compression)
		}
	}
}

// Deserializes a Go object using Gob encoding
func Deserialize(s []byte, object interface{}) error {
	// Get the bytes for the Gob-encoded object
	data, _, err := DeserializeData(s, true)
	if err != nil {
		return err
	}

	// Decode the bytes
	buffer := bytes.NewBuffer(data)
	dec := gob.NewDecoder(buffer)
	return dec.Decode(object)
}
