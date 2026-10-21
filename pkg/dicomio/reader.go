package dicomio

import (
	"bufio"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/suyashkumar/dicom/pkg/charset"
	"golang.org/x/text/encoding"
)

var (
	// ErrorInsufficientBytesLeft indicates there are not enough bytes left in
	// the current buffer (or enough bytes left until the currently set limit)
	// to complete the operation.
	ErrorInsufficientBytesLeft = errors.New("not enough bytes left until buffer limit to complete this operation")
)

// LimitReadUntilEOF is a special dicomio.Reader limit indicating that there is no hard limit and the
// Reader should read until EOF.
const LimitReadUntilEOF = -9999

type Reader struct {
	in         *bufio.Reader
	bo         binary.ByteOrder
	implicit   bool
	limit      int64
	bytesRead  int64
	limitStack []int64
	// exactLimit is false when the limit is not a count of bytes the input
	// holds: LimitReadUntilEOF, or a limit on deflated input.
	exactLimit bool
	// cs represents the CodingSystem to use when reading the string. If a
	// particular encoding.Decoder within this CodingSystem is nil, assume
	// UTF-8.
	cs charset.CodingSystem
}

// NewReader creates and returns a new *dicomio.Reader.
func NewReader(in *bufio.Reader, bo binary.ByteOrder, limit int64) *Reader {
	return &Reader{
		in:         in,
		bo:         bo,
		limit:      limit,
		bytesRead:  0,
		exactLimit: limit != LimitReadUntilEOF,
	}
}

func (r *Reader) BytesLeftUntilLimit() int64 {
	if r.limit == LimitReadUntilEOF {
		return math.MaxInt64
	}
	return r.limit - r.bytesRead
}

// Available returns ErrorInsufficientBytesLeft when n is more than the bytes
// the Reader can still supply. With an exact limit that refuses a declared
// length before anything is allocated; AppendBytes depends on it for that. The
// limit is then the caller's size, so a caller passing a size larger than its
// stream holds can be made to allocate up to that size.
func (r *Reader) Available(n int64) error {
	if n < 0 || n > r.BytesLeftUntilLimit() {
		return ErrorInsufficientBytesLeft
	}
	return nil
}

// ExactLimit reports whether the limit counts bytes the input holds, so that
// Available bounds what a read can return.
func (r *Reader) ExactLimit() bool { return r.exactLimit }

// growChunk is the most AppendBytes allocates for a value before its bytes
// arrive when the limit is not exact.
const growChunk = 64 << 10

// ReadBytes reads exactly n bytes into a new slice, as AppendBytes does.
func (r *Reader) ReadBytes(n int64) ([]byte, error) {
	// Non-nil, so that an empty value stays empty rather than null.
	return r.AppendBytes([]byte{}, n)
}

// AppendBytes reads exactly n bytes and appends them to dst. An n past the
// limit is refused with an error wrapping ErrorInsufficientBytesLeft before
// anything is read. When dst lacks room, an exact limit allocates once;
// otherwise the buffer grows as bytes arrive: the buffer returned is at most
// about twice the bytes read, and all the buffers grown on the way about
// three times (four for a value cut short). Like io.ReadFull, it returns
// io.EOF only if no bytes were read.
func (r *Reader) AppendBytes(dst []byte, n int64) ([]byte, error) {
	if err := r.Available(n); err != nil {
		return nil, fmt.Errorf("%d bytes declared, %d left: %w", n, r.BytesLeftUntilLimit(), err)
	}
	start := int64(len(dst))
	end := start + n
	for int64(len(dst)) < end {
		if len(dst) == cap(dst) {
			want := end
			if !r.exactLimit {
				want = min(end, int64(len(dst))+max(int64(len(dst))-start, growChunk))
			}
			// Grown by hand: append's growth overshoots the target.
			grown := make([]byte, len(dst), want)
			copy(grown, dst)
			dst = grown
		}
		got, err := io.ReadFull(r, dst[len(dst):min(int64(cap(dst)), end)])
		dst = dst[:len(dst)+got]
		if err != nil {
			if err == io.EOF && int64(len(dst)) > start {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
	}
	return dst, nil
}

func (r *Reader) Read(p []byte) (int, error) {
	// Check if we've hit the limit
	if r.BytesLeftUntilLimit() <= 0 {
		if len(p) == 0 {
			return 0, nil
		}
		return 0, io.EOF
	}

	// If asking for more than we have left, just return whatever we've got left
	// TODO: return a special kind of error if this situation occurs to inform the caller
	if int64(len(p)) > r.BytesLeftUntilLimit() {
		p = p[:r.BytesLeftUntilLimit()]
	}
	n, err := r.in.Read(p)
	if n >= 0 {
		r.bytesRead += int64(n)
	}
	return n, err
}

// ReadUInt8 reads an uint8 from the underlying *Reader.
func (r *Reader) ReadUInt8() (uint8, error) {
	var out uint8
	err := binary.Read(r, r.bo, &out)
	return out, err
}

// ReadUInt16 reads an uint16 from the underlying *Reader.
func (r *Reader) ReadUInt16() (uint16, error) {
	var out uint16
	err := binary.Read(r, r.bo, &out)
	return out, err
}

// ReadUInt32 reads an uint32 from the underlying *Reader.
func (r *Reader) ReadUInt32() (uint32, error) {
	var out uint32
	err := binary.Read(r, r.bo, &out)
	return out, err
}

// ReadInt16 reads an int16 from the underlying *Reader.
func (r *Reader) ReadInt16() (int16, error) {
	var out int16
	err := binary.Read(r, r.bo, &out)
	return out, err
}

// ReadInt32 reads an int32 from the underlying *Reader.
func (r *Reader) ReadInt32() (int32, error) {
	var out int32
	err := binary.Read(r, r.bo, &out)
	return out, err
}

// ReadFloat32 reads a float32 from the underlying *Reader.
func (r *Reader) ReadFloat32() (float32, error) {
	var out float32
	err := binary.Read(r, r.bo, &out)
	return out, err
}

// ReadFloat64 reads a float64 from the underlying *Reader.
func (r *Reader) ReadFloat64() (float64, error) {
	var out float64
	err := binary.Read(r, r.bo, &out)
	return out, err
}

func internalReadString(data []byte, d *encoding.Decoder) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	if d == nil {
		// Assume UTF-8
		return string(data), nil
	}
	bytes, err := d.Bytes(data)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

// ReadString reads a string from the underlying *Reader.
func (r *Reader) ReadString(n uint32) (string, error) {
	data, err := r.ReadBytes(int64(n))
	if err != nil {
		return "", err
	}
	return internalReadString(data, r.cs.Ideographic)
}

// Skip skips the *Reader ahead by n bytes.
func (r *Reader) Skip(n int64) error {
	if r.BytesLeftUntilLimit() < n {
		// not enough left to skip
		return ErrorInsufficientBytesLeft
	}

	_, err := io.CopyN(io.Discard, r, n)

	return err
}

// PushLimit creates a limit n bytes from the current position.
func (r *Reader) PushLimit(n int64) error {
	// A negative n could land the limit on LimitReadUntilEOF, unlimiting
	// a Reader whose limit is still taken as exact.
	if n < 0 {
		return fmt.Errorf("negative limit %d: %w", n, ErrorInsufficientBytesLeft)
	}
	newLimit := r.bytesRead + n
	if newLimit > r.limit && r.limit != LimitReadUntilEOF {
		return fmt.Errorf("new limit exceeds current limit of buffer, new limit: %d, limit: %d: %w", newLimit, r.limit, ErrorInsufficientBytesLeft)
	}

	// Add current limit to the stack
	r.limitStack = append(r.limitStack, r.limit)
	r.limit = newLimit
	return nil
}

// PopLimit removes the most recent limit set, and restores the limit before
// that one.
func (r *Reader) PopLimit() {
	if r.bytesRead < r.limit && r.limit != LimitReadUntilEOF {
		// didn't read all the way to the limit, so skip over what's left.
		_ = r.Skip(r.limit - r.bytesRead)
	}
	// TODO: return an error if trying to Pop the last limit off the slice
	last := len(r.limitStack) - 1
	r.limit = r.limitStack[last]
	r.limitStack = r.limitStack[:last]
}

func (r *Reader) IsLimitExhausted() bool {
	// if limit < 0 than we should read until EOF
	return (r.BytesLeftUntilLimit() <= 0 && r.limit != LimitReadUntilEOF)
}

func (r *Reader) SetTransferSyntax(bo binary.ByteOrder, implicit bool) {
	r.bo = bo
	r.implicit = implicit
}

// SetDeflate applies deflate decompression to the underlying *Reader for all
// subsequent reads. This should be set when working with a deflated
// transfer syntax. Right now this is expected to be called once when
// parsing the top level dicom data, and there is no facility to swap
// between deflate and non-deflate reading.
// The original limit counts compressed bytes, so it is replaced with the most
// those bytes can inflate to, or LimitReadUntilEOF if there was no limit.
// Either way the limit is no longer exact.
func (r *Reader) SetDeflate() {
	r.in = bufio.NewReader(flate.NewReader(r.in))
	r.exactLimit = false
	if r.limit == LimitReadUntilEOF {
		return
	}
	// 1032:1 is deflate's maximum expansion ratio (zlib's technical details).
	const maxDeflateRatio = 1032
	left := r.limit - r.bytesRead
	if left > (math.MaxInt64-r.bytesRead)/maxDeflateRatio {
		r.limit = LimitReadUntilEOF
		return
	}
	r.limit = r.bytesRead + left*maxDeflateRatio
}

// IsImplicit returns if the currently set transfer syntax on this *Reader is
// implicit or not.
func (r *Reader) IsImplicit() bool { return r.implicit }

// SetCodingSystem sets the charset.CodingSystem to be used when ReadString
// is called.
func (r *Reader) SetCodingSystem(cs charset.CodingSystem) {
	r.cs = cs
}

// Peek reads and returns the next n bytes (if possible) without advancing the
// underlying reader.
func (r *Reader) Peek(n int) ([]byte, error) {
	return r.in.Peek(n)
}

// ByteOrder returns the current byte order.
func (r *Reader) ByteOrder() binary.ByteOrder {
	return r.bo
}
