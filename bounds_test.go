package dicom

import (
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"math/rand"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/suyashkumar/dicom/pkg/dicomio"
	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
)

// hugeVL is far more than any test input holds. It is small enough that the
// unguarded parser could actually allocate it, so a regression shows up as an
// allocation over maxBoundedAlloc rather than an out-of-memory crash.
const hugeVL = 0x10000000

// maxBoundedAlloc is the most a refused small input may allocate.
const maxBoundedAlloc = 256 << 10

// longVRs are the explicit VRs with a 2-byte reserved field and a 4-byte VL.
var longVRs = map[string]bool{"OB": true, "OD": true, "OF": true, "OL": true, "OW": true, "SQ": true, "UC": true, "UN": true, "UR": true, "UT": true}

// explicitElem encodes an explicit VR little endian element. vl is written
// as given, whatever the length of value.
func explicitElem(t tag.Tag, vr string, vl uint32, value []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, t.Group)
	binary.Write(&b, binary.LittleEndian, t.Element)
	b.WriteString(vr)
	if longVRs[vr] {
		b.Write([]byte{0, 0})
		binary.Write(&b, binary.LittleEndian, vl)
	} else {
		binary.Write(&b, binary.LittleEndian, uint16(vl))
	}
	b.Write(value)
	return b.Bytes()
}

// implicitElem encodes an implicit VR little endian element, which is also
// how items and delimiters are encoded under every transfer syntax.
func implicitElem(t tag.Tag, vl uint32, value []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, t.Group)
	binary.Write(&b, binary.LittleEndian, t.Element)
	binary.Write(&b, binary.LittleEndian, vl)
	b.Write(value)
	return b.Bytes()
}

func us(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// dicomFile wraps body in a preamble and a file meta group naming ts.
func dicomFile(ts string, body []byte) []byte {
	tsValue := []byte(ts)
	if len(tsValue)%2 == 1 {
		tsValue = append(tsValue, 0)
	}
	tsElem := explicitElem(tag.TransferSyntaxUID, "UI", uint32(len(tsValue)), tsValue)
	return cat(
		make([]byte, 128), []byte("DICM"),
		explicitElem(tag.FileMetaInformationGroupLength, "UL", 4, binary.LittleEndian.AppendUint32(nil, uint32(len(tsElem)))),
		tsElem,
		body,
	)
}

func explicitFile(body ...[]byte) []byte {
	return dicomFile(uid.ExplicitVRLittleEndian, cat(body...))
}

func implicitFile(body ...[]byte) []byte {
	return dicomFile(uid.ImplicitVRLittleEndian, cat(body...))
}

func deflatedFile(body ...[]byte) []byte {
	return dicomFile(uid.DeflatedExplicitVRLittleEndian, deflate(body...))
}

func deflate(body ...[]byte) []byte {
	var b bytes.Buffer
	w, _ := flate.NewWriter(&b, flate.BestCompression)
	w.Write(cat(body...))
	w.Close()
	return b.Bytes()
}

// nativeImage returns the elements readNativeFrames sizes PixelData from.
func nativeImage(rows, cols, bitsAllocated, samplesPerPixel uint16, numberOfFrames string) []byte {
	return cat(
		explicitElem(tag.SamplesPerPixel, "US", 2, us(samplesPerPixel)),
		explicitElem(tag.NumberOfFrames, "IS", uint32(len(numberOfFrames)), []byte(numberOfFrames)),
		explicitElem(tag.Rows, "US", 2, us(rows)),
		explicitElem(tag.Columns, "US", 2, us(cols)),
		explicitElem(tag.BitsAllocated, "US", 2, us(bitsAllocated)),
	)
}

var (
	itemUndefined     = implicitElem(tag.Item, tag.VLUndefinedLength, nil)
	itemDelimitation  = implicitElem(tag.ItemDelimitationItem, 0, nil)
	seqDelimitation   = implicitElem(tag.SequenceDelimitationItem, 0, nil)
	privateBytesTag   = tag.Tag{Group: 0x0009, Element: 0x1010}
	privateUnknownTag = tag.Tag{Group: 0x0009, Element: 0x1011}
)

// nestedUndefined nests depth undefined-length sequences of vr, each holding
// one undefined-length item.
func nestedUndefined(depth int, vr string) []byte {
	t := tag.ContentSequence
	if vr == "UN" {
		t = privateUnknownTag
	}
	open := cat(explicitElem(t, vr, tag.VLUndefinedLength, nil), itemUndefined)
	closing := cat(itemDelimitation, seqDelimitation)
	return cat(bytes.Repeat(open, depth), bytes.Repeat(closing, depth))
}

// nestedDefined nests depth defined-length sequences, each holding one
// defined-length item.
func nestedDefined(depth int) []byte {
	inner := explicitElem(tag.PatientName, "PN", 4, []byte("ABCD"))
	for i := 0; i < depth; i++ {
		item := implicitElem(tag.Item, uint32(len(inner)), inner)
		inner = explicitElem(tag.ContentSequence, "SQ", uint32(len(item)), item)
	}
	return inner
}

func bytesAllocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// parseBounded parses data with a known size, failing the test on a panic or
// on an allocation over maxAlloc.
func parseBounded(t *testing.T, data []byte, maxAlloc uint64, opts ...ParseOption) (Dataset, error) {
	t.Helper()
	var ds Dataset
	var err error
	allocated := bytesAllocated(func() {
		ds, err = Parse(bytes.NewReader(data), int64(len(data)), nil, opts...)
	})
	if allocated > maxAlloc {
		t.Errorf("Parse of a %d byte input allocated %d bytes, want at most %d", len(data), allocated, maxAlloc)
	}
	return ds, err
}

// boundsCases are inputs whose declared sizes or nesting exceed what they
// hold. FuzzParse seeds from them too.
var boundsCases = []struct {
	name    string
	data    []byte
	opts    []ParseOption
	wantErr error
}{
	{
		name:    "string longer than input",
		data:    explicitFile(explicitElem(tag.PatientName, "PN", 0xfffe, []byte("AB"))),
		wantErr: dicomio.ErrorInsufficientBytesLeft,
	},
	{
		name:    "long text longer than input",
		data:    explicitFile(explicitElem(tag.TextValue, "UT", hugeVL, []byte("AB"))),
		wantErr: dicomio.ErrorInsufficientBytesLeft,
	},
	{
		name:    "date longer than input",
		data:    explicitFile(explicitElem(tag.StudyDate, "DA", 0xfffe, []byte("2024"))),
		wantErr: dicomio.ErrorInsufficientBytesLeft,
	},
	{
		name:    "OB longer than input",
		data:    explicitFile(explicitElem(privateBytesTag, "OB", hugeVL, []byte{1, 2})),
		wantErr: dicomio.ErrorInsufficientBytesLeft,
	},
	{
		name:    "OW longer than input",
		data:    explicitFile(explicitElem(privateBytesTag, "OW", hugeVL, []byte{1, 2})),
		wantErr: dicomio.ErrorInsufficientBytesLeft,
	},
	{
		name:    "UN longer than input",
		data:    explicitFile(explicitElem(privateUnknownTag, "UN", hugeVL, []byte{1, 2})),
		wantErr: dicomio.ErrorInsufficientBytesLeft,
	},
	{
		name:    "US longer than input",
		data:    explicitFile(explicitElem(tag.Rows, "US", 0xfffe, us(1))),
		wantErr: dicomio.ErrorInsufficientBytesLeft,
	},
	{
		name:    "implicit US longer than input",
		data:    implicitFile(implicitElem(tag.Rows, hugeVL, us(1))),
		wantErr: dicomio.ErrorInsufficientBytesLeft,
	},
	{
		name:    "FD longer than input",
		data:    explicitFile(explicitElem(tag.TimeRange, "FD", 0xfff8, make([]byte, 8))),
		wantErr: dicomio.ErrorInsufficientBytesLeft,
	},
	{
		name:    "implicit FD longer than input",
		data:    implicitFile(implicitElem(tag.TimeRange, hugeVL, make([]byte, 8))),
		wantErr: dicomio.ErrorInsufficientBytesLeft,
	},
	{
		name: "undefined length OB item longer than input",
		data: explicitFile(
			explicitElem(privateBytesTag, "OB", tag.VLUndefinedLength, nil),
			implicitElem(tag.Item, hugeVL, []byte{1, 2}),
		),
		wantErr: dicomio.ErrorInsufficientBytesLeft,
	},
	{
		name:    "unprocessed PixelData longer than input",
		data:    explicitFile(explicitElem(tag.PixelData, "OW", hugeVL, []byte{1, 2})),
		opts:    []ParseOption{SkipProcessingPixelDataValue()},
		wantErr: dicomio.ErrorInsufficientBytesLeft,
	},
	{
		name: "mismatched PixelData longer than input",
		data: explicitFile(
			nativeImage(2, 2, 8, 1, "1 "),
			explicitElem(tag.PixelData, "OW", hugeVL, []byte{1, 2}),
		),
		opts:    []ParseOption{AllowMismatchPixelDataLength()},
		wantErr: dicomio.ErrorInsufficientBytesLeft,
	},
	{
		name: "native PixelData matching its dimensions but longer than input",
		data: explicitFile(
			nativeImage(4096, 4096, 16, 1, "8 "),
			explicitElem(tag.PixelData, "OW", 4096*4096*2*8, []byte{1, 2}),
		),
		wantErr: dicomio.ErrorInsufficientBytesLeft,
	},
	{
		name: "native PixelData dimensions overflowing 64 bits",
		data: explicitFile(
			nativeImage(65535, 65535, 32, 65535, "2147483647"),
			explicitElem(tag.PixelData, "OW", 4, []byte{1, 2, 3, 4}),
		),
		wantErr: ErrorMismatchPixelDataLength,
	},
	{
		name: "native PixelData with no Rows value",
		data: explicitFile(
			explicitElem(tag.Rows, "US", 0, nil),
			explicitElem(tag.PixelData, "OW", 4, []byte{1, 2, 3, 4}),
		),
		wantErr: ErrorInvalidPixelDimensions,
	},
	{
		name: "native PixelData with zero-byte frames",
		data: explicitFile(
			nativeImage(1, 1, 1, 1, "2147483647"),
			explicitElem(tag.PixelData, "OW", 0, nil),
		),
		wantErr: ErrorInvalidPixelDimensions,
	},
	{
		name:    "deflated string longer than the input can inflate to",
		data:    deflatedFile(explicitElem(tag.TextValue, "UT", 0xfffffff0, []byte("AB"))),
		wantErr: dicomio.ErrorInsufficientBytesLeft,
	},
	{
		name:    "undefined length sequences nested too deep",
		data:    explicitFile(nestedUndefined(DefaultMaxSequenceDepth+1, "SQ")),
		wantErr: ErrorSequenceTooDeep,
	},
	{
		name:    "undefined length UN sequences nested too deep",
		data:    explicitFile(nestedUndefined(DefaultMaxSequenceDepth+1, "UN")),
		wantErr: ErrorSequenceTooDeep,
	},
	{
		name:    "defined length sequences nested too deep",
		data:    explicitFile(nestedDefined(DefaultMaxSequenceDepth + 1)),
		wantErr: ErrorSequenceTooDeep,
	},
	{
		name:    "unterminated sequences nested ten thousand deep",
		data:    explicitFile(bytes.Repeat(cat(explicitElem(tag.ContentSequence, "SQ", tag.VLUndefinedLength, nil), itemUndefined), 10000)),
		wantErr: ErrorSequenceTooDeep,
	},
	{
		name:    "sequences nested deeper than a custom limit",
		data:    explicitFile(nestedUndefined(5, "SQ")),
		opts:    []ParseOption{MaxSequenceDepth(4)},
		wantErr: ErrorSequenceTooDeep,
	},
}

func TestParse_RefusesSizesAndNestingTheInputDoesNotHold(t *testing.T) {
	for _, tc := range boundsCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseBounded(t, tc.data, maxBoundedAlloc, tc.opts...)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Parse error = %v, want %v", err, tc.wantErr)
			}
			if err != nil && len(err.Error()) > 1024 {
				t.Errorf("Parse error message is %d bytes, want at most 1024", len(err.Error()))
			}
		})
	}
}

func TestParse_EncapsulatedFrameLongerThanInput(t *testing.T) {
	// A frame read error ends encapsulated PixelData without failing the
	// parse, as upstream does for a truncated file: the frames before it are
	// kept and nothing after is read as elements.
	data := explicitFile(
		explicitElem(tag.PixelData, "OB", tag.VLUndefinedLength, nil),
		implicitElem(tag.Item, 0, nil),
		implicitElem(tag.Item, 4, []byte{1, 2, 3, 4}),
		implicitElem(tag.Item, hugeVL, bytes.Repeat([]byte{0xff, 0xd8}, 16)),
	)
	ds, err := parseBounded(t, data, maxBoundedAlloc)
	if err != nil {
		t.Fatalf("Parse error = %v", err)
	}
	e, err := ds.FindElementByTag(tag.PixelData)
	if err != nil {
		t.Fatal(err)
	}
	frames := MustGetPixelDataInfo(e.Value).Frames
	if len(frames) != 1 || !bytes.Equal(frames[0].EncapsulatedData.Data, []byte{1, 2, 3, 4}) {
		t.Errorf("frames = %v, want the one complete frame", frames)
	}
	if last := ds.Elements[len(ds.Elements)-1]; last.Tag != tag.PixelData {
		t.Errorf("last element = %v, want PixelData", last.Tag)
	}
}

// Zero and negative dimensions are wrong sizes like any other: they fail the
// size check, or with AllowMismatchPixelDataLength become error PixelData.
func TestParse_ZeroAndNegativeDimensions(t *testing.T) {
	cases := []struct {
		name         string
		data         []byte
		opts         []ParseOption
		wantErr      error
		wantParseErr error
	}{
		{
			name: "zero frames and no pixel data",
			data: explicitFile(nativeImage(2, 2, 8, 1, "0 "), explicitElem(tag.PixelData, "OW", 0, nil)),
		},
		{
			name:    "zero rows",
			data:    explicitFile(nativeImage(0, 2, 8, 1, "1 "), explicitElem(tag.PixelData, "OW", 4, []byte{1, 2, 3, 4})),
			wantErr: ErrorMismatchPixelDataLength,
		},
		{
			name:         "zero rows, mismatch allowed",
			data:         explicitFile(nativeImage(0, 2, 8, 1, "1 "), explicitElem(tag.PixelData, "OW", 4, []byte{1, 2, 3, 4})),
			opts:         []ParseOption{AllowMismatchPixelDataLength()},
			wantParseErr: ErrorMismatchPixelDataLength,
		},
		{
			name:    "negative frames",
			data:    explicitFile(nativeImage(2, 2, 8, 1, "-1"), explicitElem(tag.PixelData, "OW", 4, []byte{1, 2, 3, 4})),
			wantErr: ErrorMismatchPixelDataLength,
		},
		{
			name:    "negative frames and no pixel data",
			data:    explicitFile(nativeImage(0, 2, 8, 1, "-1"), explicitElem(tag.PixelData, "OW", 0, nil)),
			wantErr: ErrorInvalidPixelDimensions,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ds, err := parseBounded(t, tc.data, maxBoundedAlloc, tc.opts...)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Parse error = %v, want %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			e, err := ds.FindElementByTag(tag.PixelData)
			if err != nil {
				t.Fatal(err)
			}
			if got := MustGetPixelDataInfo(e.Value).ParseErr; !errors.Is(got, tc.wantParseErr) {
				t.Errorf("PixelData ParseErr = %v, want %v", got, tc.wantParseErr)
			}
		})
	}
}

func TestParse_MaxSequenceDepthExtremes(t *testing.T) {
	if _, err := parseBounded(t, explicitFile(nestedUndefined(3, "SQ")), maxBoundedAlloc, MaxSequenceDepth(math.MaxInt)); err != nil {
		t.Errorf("MaxSequenceDepth(math.MaxInt): Parse error = %v", err)
	}
	if _, err := parseBounded(t, explicitFile(nestedUndefined(1, "SQ")), maxBoundedAlloc, MaxSequenceDepth(0)); !errors.Is(err, ErrorSequenceTooDeep) {
		t.Errorf("MaxSequenceDepth(0): Parse error = %v, want %v", err, ErrorSequenceTooDeep)
	}
}

func TestParse_MalformedHeaderValuesReturnErrors(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{
			name: "TransferSyntaxUID that is not a string",
			data: cat(
				make([]byte, 128), []byte("DICM"),
				explicitElem(tag.FileMetaInformationGroupLength, "UL", 4, binary.LittleEndian.AppendUint32(nil, 12)),
				explicitElem(tag.TransferSyntaxUID, "UL", 4, []byte{1, 2, 3, 4}),
			),
		},
		{
			name: "unknown TransferSyntaxUID",
			data: dicomFile("1.2.3.4", nil),
		},
		{
			name: "MetaElementGroupLength with no value",
			data: cat(make([]byte, 128), []byte("DICM"), explicitElem(tag.FileMetaInformationGroupLength, "UL", 0, nil)),
		},
		{
			name: "non-item element in a defined length sequence",
			data: explicitFile(explicitElem(tag.ContentSequence, "SQ", 12, explicitElem(tag.PatientName, "PN", 4, []byte("ABCD")))),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseBounded(t, tc.data, maxBoundedAlloc); err == nil {
				t.Error("Parse returned no error")
			}
		})
	}
}

func TestParse_SpecificCharacterSetOfTheWrongType(t *testing.T) {
	for _, vr := range []string{"US", "OB", "UN"} {
		data := explicitFile(
			explicitElem(tag.SpecificCharacterSet, vr, 2, us(1)),
			explicitElem(tag.PatientName, "PN", 4, []byte("ABCD")),
		)
		for _, opts := range [][]ParseOption{nil, {AllowUnknownSpecificCharacterSet()}} {
			ds, err := parseBounded(t, data, maxBoundedAlloc, opts...)
			if err != nil {
				t.Errorf("%s value, %d options: Parse error = %v, want the default character set", vr, len(opts), err)
				continue
			}
			if name, err := ds.FindElementByTag(tag.PatientName); err != nil || MustGetStrings(name.Value)[0] != "ABCD" {
				t.Errorf("%s value, %d options: PatientName = %v (%v), want ABCD", vr, len(opts), name, err)
			}
		}
	}
}

func TestParse_SpecificCharacterSetAsBytesApplies(t *testing.T) {
	latin1Name := []byte{'J', 'O', 'S', 0xc9} // JOSÉ in ISO_IR 100
	cases := map[string]struct {
		value []byte
		want  string
	}{
		"ISO_IR 100":                {[]byte("ISO_IR 100"), "JOSÉ"},
		"padded with a NUL":         {[]byte("ISO_IR 100\x00"), "JOSÉ"},
		"padded with a space":       {[]byte("ISO_IR 100 "), "JOSÉ"},
		"first of two values":       {[]byte("ISO_IR 100\\ISO_IR 100 "), "JOSÉ"},
		"unknown takes the default": {[]byte("NOT A SET "), string(latin1Name)},
	}
	for name, tc := range cases {
		for _, vr := range []string{"OB", "UN"} {
			data := explicitFile(
				explicitElem(tag.SpecificCharacterSet, vr, uint32(len(tc.value)), tc.value),
				explicitElem(tag.PatientName, "PN", uint32(len(latin1Name)), latin1Name),
			)
			ds, err := parseBounded(t, data, maxBoundedAlloc)
			if err != nil {
				t.Errorf("%s as %s: Parse error = %v", name, vr, err)
				continue
			}
			if e, err := ds.FindElementByTag(tag.PatientName); err != nil || MustGetStrings(e.Value)[0] != tc.want {
				t.Errorf("%s as %s: PatientName = %v (%v), want %q", name, vr, e, err, tc.want)
			}
		}
	}
}

func TestParse_NestingWithinLimit(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		opts []ParseOption
	}{
		{name: "undefined length at the default limit", data: explicitFile(nestedUndefined(DefaultMaxSequenceDepth, "SQ"))},
		{name: "defined length at the default limit", data: explicitFile(nestedDefined(DefaultMaxSequenceDepth))},
		{name: "UN at the default limit", data: explicitFile(nestedUndefined(DefaultMaxSequenceDepth, "UN"))},
		{name: "at a custom limit", data: explicitFile(nestedUndefined(4, "SQ")), opts: []ParseOption{MaxSequenceDepth(4)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseBounded(t, tc.data, maxBoundedAlloc, tc.opts...); err != nil {
				t.Errorf("Parse error = %v", err)
			}
		})
	}
}

// parseGoldens are sha256 sums of the JSON encoding of each testdata file as
// parsed by the upstream v1.1.0 release.
var parseGoldens = map[string]string{
	"1.dcm": "54cf36e5c467500f344d5693781d041192235ebacd9a4ae3632374e8c181f065",
	"2.dcm": "c45d6caead127f5e9d047b3e091f24a9ff7060aada55c1fb8188f3791eb5b726",
	"3.dcm": "c3f8074c86f09c661d95990e6dd3ea0285296653eace3148922785fd81c579a6",
	"4.dcm": "9c45da6afd581c442cb27afd4a956a51c526a197ac4de61e3a5e59f0283e28ad",
	"5.dcm": "2b94790574a2c2e534736d30dec2ded27ea21c33d8fd777d57d8cc5b733eef14",
	"6.dcm": "81151c17a349e1fb943fb3cdb5b99d64ac78fce78190b01b9e471cee433e1d49",
}

func TestParse_TestdataUnchangedFromUpstream(t *testing.T) {
	for name, want := range parseGoldens {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile("./testdata/" + name)
			if err != nil {
				t.Fatal(err)
			}
			ds, err := Parse(bytes.NewReader(data), int64(len(data)), nil)
			if err != nil {
				t.Fatalf("Parse error = %v", err)
			}
			j, err := json.Marshal(ds)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(j)
			if got := hex.EncodeToString(sum[:]); got != want {
				t.Errorf("parsed JSON sha256 = %s, want %s", got, want)
			}
		})
	}
}

// incompressible returns n bytes deflate cannot shrink, so that a deflated
// input's limit (1032 times its compressed bytes) is far above what it holds.
func incompressible(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(1)).Read(b)
	return b
}

// untrustedLimitCases declare lengths within the limit of a reader that
// cannot bound them: one with no size, or one reading deflated input.
var untrustedLimitCases = []struct {
	name string
	body []byte
	opts []ParseOption
	// parses is set where upstream ends the parse without an error: ints
	// and floats stop at the end of the input, and encapsulated PixelData
	// ends its frames.
	parses bool
}{
	{name: "string", body: explicitElem(tag.TextValue, "UT", 4<<20, incompressible(8<<10))},
	{name: "OB", body: explicitElem(privateBytesTag, "OB", 4<<20, incompressible(8<<10))},
	{name: "OW", body: explicitElem(privateBytesTag, "OW", 4<<20, incompressible(8<<10))},
	{name: "UN", body: explicitElem(privateUnknownTag, "UN", 4<<20, incompressible(8<<10))},
	{name: "US", body: explicitElem(tag.Rows, "US", 0xfffe, incompressible(8<<10)), parses: true},
	{name: "FD", body: explicitElem(tag.TimeRange, "FD", 0xfff8, incompressible(8<<10)), parses: true},
	{
		name: "undefined length OB item",
		body: cat(explicitElem(privateBytesTag, "OB", tag.VLUndefinedLength, nil), implicitElem(tag.Item, 4<<20, incompressible(8<<10))),
	},
	{
		name:   "encapsulated frame",
		body:   cat(explicitElem(tag.PixelData, "OB", tag.VLUndefinedLength, nil), implicitElem(tag.Item, 0, nil), implicitElem(tag.Item, 4<<20, incompressible(8<<10))),
		parses: true,
	},
	{
		name: "unprocessed PixelData",
		body: explicitElem(tag.PixelData, "OW", 4<<20, incompressible(8<<10)),
		opts: []ParseOption{SkipProcessingPixelDataValue()},
	},
	{
		name: "mismatched PixelData",
		body: cat(nativeImage(2, 2, 8, 1, "1 "), explicitElem(tag.PixelData, "OW", 4<<20, incompressible(8<<10))),
		opts: []ParseOption{AllowMismatchPixelDataLength()},
	},
	{
		name: "native PixelData matching its dimensions",
		body: cat(nativeImage(1024, 1024, 8, 1, "4 "), explicitElem(tag.PixelData, "OW", 4<<20, incompressible(8<<10))),
	},
}

// A declared length the reader cannot check allocates no more than a few
// times what the input actually holds.
func TestParse_UntrustedLimitAllocatesFromBytesRead(t *testing.T) {
	for _, tc := range untrustedLimitCases {
		t.Run(tc.name+"/until EOF", func(t *testing.T) {
			data := explicitFile(tc.body)
			var err error
			allocated := bytesAllocated(func() {
				_, err = ParseUntilEOF(bytes.NewReader(data), nil, tc.opts...)
			})
			if limit := uint64(maxBoundedAlloc + 3*len(data)); allocated > limit {
				t.Errorf("ParseUntilEOF of a %d byte input allocated %d bytes, want at most %d", len(data), allocated, limit)
			}
			if (err == nil) != tc.parses {
				t.Errorf("ParseUntilEOF error = %v", err)
			}
		})
		t.Run(tc.name+"/deflated", func(t *testing.T) {
			data := deflatedFile(tc.body)
			_, err := parseBounded(t, data, uint64(maxBoundedAlloc+3*len(tc.body)), tc.opts...)
			if (err == nil) != tc.parses {
				t.Errorf("Parse error = %v", err)
			}
		})
	}
}

func TestParse_DeflatedValuesParse(t *testing.T) {
	value := bytes.Repeat([]byte("ABCDEFGH"), 64<<10)
	data := deflatedFile(explicitElem(privateBytesTag, "OB", uint32(len(value)), value), explicitElem(tag.PatientName, "PN", 4, []byte("ABCD")))
	ds, err := parseBounded(t, data, uint64(maxBoundedAlloc+3*len(value)))
	if err != nil {
		t.Fatalf("Parse error = %v", err)
	}
	e, err := ds.FindElementByTag(privateBytesTag)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Value.GetValue().([]byte); !bytes.Equal(got, value) {
		t.Errorf("OB value is %d bytes, want the %d written", len(got), len(value))
	}
	if _, err := ds.FindElementByTag(tag.PatientName); err != nil {
		t.Errorf("element after the OB: %v", err)
	}
}

// The allowance is shared by every PixelData element of one parse.
func TestParse_NativeAllowanceIsPerParse(t *testing.T) {
	const frames = 80000
	px := explicitElem(tag.PixelData, "OW", frames, make([]byte, frames))
	data := explicitFile(nativeImage(1, 1, 8, 1, "80000"), px, px, px, px)
	limit := uint64(fuzzAllocBase + fuzzAllocPerByte*len(data) + nativeAllocAllowance*9/8)
	if _, err := parseBounded(t, data, limit); !errors.Is(err, ErrorNativeFramesTooSmall) {
		t.Errorf("Parse error = %v, want %v", err, ErrorNativeFramesTooSmall)
	}
	allocated := bytesAllocated(func() { _, _ = ParseUntilEOF(bytes.NewReader(data), nil) })
	if allocated > limit {
		t.Errorf("ParseUntilEOF allocated %d bytes, want at most %d", allocated, limit)
	}
}

func TestParse_NativeFramesTooSmallAreRefused(t *testing.T) {
	const frames = 1 << 20
	cases := []struct {
		name string
		data []byte
	}{
		{
			name: "one-byte 8-bit frames",
			data: explicitFile(nativeImage(1, 1, 8, 1, "1048576"), explicitElem(tag.PixelData, "OW", frames, make([]byte, frames))),
		},
		{
			name: "one-byte 1-bit frames",
			data: explicitFile(nativeImage(8, 1, 1, 1, "1048576"), explicitElem(tag.PixelData, "OW", frames, make([]byte, frames))),
		},
		{
			name: "one-pixel 16-bit frames",
			data: explicitFile(nativeImage(1, 1, 16, 1, "524288"), explicitElem(tag.PixelData, "OW", frames, make([]byte, frames))),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseBounded(t, tc.data, maxBoundedAlloc)
			if !errors.Is(err, ErrorNativeFramesTooSmall) {
				t.Errorf("Parse error = %v, want %v", err, ErrorNativeFramesTooSmall)
			}
		})
	}
}

// The allocation multiples nativeAllocMultiple documents, at the smallest
// frames each accepts.
func TestParse_NativeFramesAllocationMultiple(t *testing.T) {
	cases := []struct {
		name     string
		image    []byte
		vl       int
		multiple int
		frames   int
	}{
		{name: "8-bit 4x6", image: nativeImage(4, 6, 8, 1, "4096"), vl: 24 * 4096, multiple: 10},
		{name: "8-bit 1x23, many frames", image: nativeImage(1, 23, 8, 1, "200000"), vl: 23 * 200000, multiple: 10},
		{name: "16-bit 2x6", image: nativeImage(2, 6, 16, 1, "200000"), vl: 24 * 200000, multiple: 10},
		{name: "32-bit 1x6", image: nativeImage(1, 6, 32, 1, "4096"), vl: 24 * 4096, multiple: 10},
		{name: "8-bit RGB 512x512", image: nativeImage(512, 512, 8, 3, "2 "), vl: 512 * 512 * 3 * 2, multiple: 10},
		{name: "1-bit 24x8", image: nativeImage(24, 8, 1, 1, "4096"), vl: 24 * 4096, multiple: 73},
		{name: "1-bit 16x16 segmentation", image: nativeImage(16, 16, 1, 1, "2000"), vl: 32 * 2000, multiple: 73},
		{name: "tiny frames within the allowance", image: nativeImage(1, 1, 8, 1, "300 "), vl: 300, multiple: 10, frames: 300},
		{name: "1-bit 4x4 segmentation, 500 frames", image: nativeImage(4, 4, 1, 1, "500 "), vl: 2 * 500, multiple: 73, frames: 500},
		{name: "16-bit 1x8, 2000 frames", image: nativeImage(1, 8, 16, 1, "2000"), vl: 16 * 2000, multiple: 10, frames: 2000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := explicitFile(tc.image, explicitElem(tag.PixelData, "OW", uint32(tc.vl), make([]byte, tc.vl)))
			// 64 KiB covers the rest of the parse.
			// Frames past the multiple cost their overhead, samples and
			// frame buffer, from the allowance.
			limit := uint64(max(tc.multiple*tc.vl, tc.frames*nativeFrameOverhead+(tc.multiple-8)*tc.vl) + 128<<10)
			if _, err := parseBounded(t, data, limit); err != nil {
				t.Errorf("Parse error = %v", err)
			}
		})
	}
}

// Native frames are allocated as their bytes arrive, so a parse with no exact
// limit costs what an exact one does, and each frame reaches the frame
// channel once read.
func TestParse_ZeroFramesParse(t *testing.T) {
	for _, bits := range []uint16{1, 8, 16} {
		data := explicitFile(nativeImage(512, 512, bits, 1, "0 "), explicitElem(tag.PixelData, "OW", 0, nil))
		ds, err := parseBounded(t, data, maxBoundedAlloc)
		if err != nil {
			t.Errorf("%d bits: Parse error = %v", bits, err)
			continue
		}
		px, err := ds.FindElementByTag(tag.PixelData)
		if err != nil {
			t.Fatal(err)
		}
		if frames := MustGetPixelDataInfo(px.Value).Frames; frames == nil || len(frames) != 0 {
			t.Errorf("%d bits: Frames = %#v, want empty and non-nil", bits, frames)
		}
	}
}

func TestParse_NegativeGroupLengthIsRefused(t *testing.T) {
	// -(9999 + bytes read) would land the limit on LimitReadUntilEOF.
	negative := int32(-10143)
	for _, vr := range []string{"SL", "SS"} {
		length := binary.LittleEndian.AppendUint32(nil, uint32(negative))
		if vr == "SS" {
			length = us(uint16(int16(negative)))
		}
		data := cat(
			make([]byte, 128), []byte("DICM"),
			explicitElem(tag.FileMetaInformationGroupLength, vr, uint32(len(length)), length),
			explicitElem(tag.FileMetaInformationVersion, "OB", 256<<20, []byte{0, 1}),
		)
		if _, err := parseBounded(t, data, maxBoundedAlloc); !errors.Is(err, ErrorMetaElementGroupLength) {
			t.Errorf("%s: Parse error = %v, want %v", vr, err, ErrorMetaElementGroupLength)
		}
	}
}

func TestParse_NativeFramesAllocateAsRead(t *testing.T) {
	const vl = 8 << 20
	body := cat(nativeImage(512, 512, 16, 1, "16"), explicitElem(tag.PixelData, "OW", vl, make([]byte, vl)))
	// The samples, a frame buffer grown as its bytes arrive, and 1 MiB for the
	// rest.
	const limit = vl + 2*512*512*2 + 1<<20
	t.Run("until EOF", func(t *testing.T) {
		data := explicitFile(body)
		allocated := bytesAllocated(func() {
			if _, err := ParseUntilEOF(bytes.NewReader(data), nil); err != nil {
				t.Errorf("ParseUntilEOF error = %v", err)
			}
		})
		if allocated > limit {
			t.Errorf("ParseUntilEOF allocated %d bytes, want at most %d", allocated, limit)
		}
	})
	t.Run("deflated", func(t *testing.T) {
		if _, err := parseBounded(t, deflatedFile(body), limit); err != nil {
			t.Errorf("Parse error = %v", err)
		}
	})
	t.Run("8-bit until EOF", func(t *testing.T) {
		data := explicitFile(nativeImage(512, 512, 8, 1, "32"), explicitElem(tag.PixelData, "OW", vl, make([]byte, vl)))
		// The frames, one frame buffer grown as its bytes arrive, and 1 MiB.
		const limit = vl + 3*512*512 + 1<<20
		allocated := bytesAllocated(func() {
			if _, err := ParseUntilEOF(bytes.NewReader(data), nil); err != nil {
				t.Errorf("ParseUntilEOF error = %v", err)
			}
		})
		if allocated > limit {
			t.Errorf("ParseUntilEOF allocated %d bytes, want at most %d", allocated, limit)
		}
	})
	t.Run("an exact limit refuses a truncation before any frame", func(t *testing.T) {
		const frameBytes = 512 * 512 * 2
		data := explicitFile(nativeImage(512, 512, 16, 1, "4 "), explicitElem(tag.PixelData, "OW", 4*frameBytes, make([]byte, 2*frameBytes+10)))
		fc := make(chan *frame.Frame, 4)
		_, err := Parse(bytes.NewReader(data), int64(len(data)), fc)
		if !errors.Is(err, dicomio.ErrorInsufficientBytesLeft) {
			t.Errorf("Parse error = %v, want %v", err, dicomio.ErrorInsufficientBytesLeft)
		}
		if got := len(fc); got != 0 {
			t.Errorf("frame channel received %d frames, want none", got)
		}
	})
	t.Run("frames before a truncation reach the channel", func(t *testing.T) {
		const frameBytes = 512 * 512 * 2
		data := explicitFile(nativeImage(512, 512, 16, 1, "4 "), explicitElem(tag.PixelData, "OW", 4*frameBytes, make([]byte, 2*frameBytes+10)))
		fc := make(chan *frame.Frame, 4)
		_, err := ParseUntilEOF(bytes.NewReader(data), fc)
		if err == nil {
			t.Fatal("ParseUntilEOF of truncated PixelData succeeded")
		}
		if got := len(fc); got != 2 {
			t.Errorf("frame channel received %d frames, want the 2 complete ones", got)
		}
	})
}

// Integer and float values start with capacity for exactly the values vl
// holds: an int or float64 per value, 8 bytes, against the 2, 4 or 8 bytes
// each value is read from. binary.Read adds about a byte per byte read.
func TestParse_IntAndFloatCapacityByValueSize(t *testing.T) {
	const vl = 1 << 20
	cases := []struct {
		name     string
		tag      tag.Tag
		multiple int
	}{
		{name: "UL", tag: tag.SimpleFrameList, multiple: 3},
		{name: "AT", tag: tag.SelectorATValue, multiple: 5},
		{name: "FD", tag: tag.TimeRange, multiple: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := implicitFile(implicitElem(tc.tag, vl, make([]byte, vl)))
			if _, err := parseBounded(t, data, uint64(tc.multiple*vl+maxBoundedAlloc)); err != nil {
				t.Errorf("Parse error = %v", err)
			}
		})
	}
}

func TestParse_IntAndFloatErrorsNameTheElement(t *testing.T) {
	for name, data := range map[string][]byte{
		"UL": implicitFile(implicitElem(tag.SimpleFrameList, 1<<20, make([]byte, 8))),
		"FD": implicitFile(implicitElem(tag.TimeRange, 1<<20, make([]byte, 8))),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseBounded(t, data, maxBoundedAlloc)
			if !errors.Is(err, dicomio.ErrorInsufficientBytesLeft) {
				t.Fatalf("Parse error = %v, want %v", err, dicomio.ErrorInsufficientBytesLeft)
			}
			if msg := err.Error(); !strings.Contains(msg, "(0008,") || !strings.Contains(msg, " "+name+" value") {
				t.Errorf("Parse error %q names neither the element's tag nor its VR", msg)
			}
		})
	}
}

func TestParse_ExplicitVRLittleEndianTransferSyntaxes(t *testing.T) {
	for _, ts := range []string{
		"1.2.840.10008.1.2.4.50", "1.2.840.10008.1.2.4.70", "1.2.840.10008.1.2.4.90", "1.2.840.10008.1.2.5", // in the uid list
		"1.2.840.10008.1.2.4.201", "1.2.840.10008.1.2.4.202", "1.2.840.10008.1.2.4.203", "1.2.840.10008.1.2.4.204", // HTJ2K
		"1.2.840.10008.1.2.4.110", "1.2.840.10008.1.2.4.111", "1.2.840.10008.1.2.4.112", // JPEG XL
		"1.2.840.10008.1.2.1.98", "1.2.840.10008.1.2.8.1", "1.2.840.10008.1.2.4.999",
	} {
		t.Run(ts, func(t *testing.T) {
			data := dicomFile(ts, cat(
				explicitElem(tag.PatientName, "PN", 4, []byte("ABCD")),
				explicitElem(tag.PixelData, "OB", tag.VLUndefinedLength, nil),
				implicitElem(tag.Item, 0, nil),
				implicitElem(tag.Item, 4, []byte{1, 2, 3, 4}),
				seqDelimitation,
			))
			ds, err := parseBounded(t, data, maxBoundedAlloc)
			if err != nil {
				t.Fatalf("Parse error = %v", err)
			}
			e, err := ds.FindElementByTag(tag.PatientName)
			if err != nil {
				t.Fatal(err)
			}
			if got := MustGetStrings(e.Value); len(got) != 1 || got[0] != "ABCD" {
				t.Errorf("PatientName = %v, want ABCD", got)
			}
			px, err := ds.FindElementByTag(tag.PixelData)
			if err != nil {
				t.Fatal(err)
			}
			if frames := MustGetPixelDataInfo(px.Value).Frames; len(frames) != 1 {
				t.Errorf("got %d frames, want 1", len(frames))
			}
		})
	}
}

func TestParse_DatasetDeflatingTransferSyntaxesInflate(t *testing.T) {
	for _, ts := range []string{"1.2.840.10008.1.2.4.95", "1.2.840.10008.1.2.4.205"} {
		t.Run(ts, func(t *testing.T) {
			data := dicomFile(ts, deflate(explicitElem(tag.PatientName, "PN", 4, []byte("ABCD"))))
			ds, err := parseBounded(t, data, maxBoundedAlloc)
			if err != nil {
				t.Fatalf("Parse error = %v", err)
			}
			e, err := ds.FindElementByTag(tag.PatientName)
			if err != nil {
				t.Fatal(err)
			}
			if got := MustGetStrings(e.Value); len(got) != 1 || got[0] != "ABCD" {
				t.Errorf("PatientName = %v, want ABCD", got)
			}
		})
	}
}

func TestParse_NonStandardTransferSyntaxIsRefused(t *testing.T) {
	data := dicomFile("1.2.3.4", explicitElem(tag.PatientName, "PN", 4, []byte("ABCD")))
	if _, err := parseBounded(t, data, maxBoundedAlloc); err == nil || !strings.Contains(err.Error(), "1.2.3.4") {
		t.Errorf("Parse error = %v, want one naming 1.2.3.4", err)
	}
}

func TestDatasetTransferSyntax_JPIPReferencedDeflate(t *testing.T) {
	ds := Dataset{Elements: []*Element{
		mustNewElement(tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.1.2"}),
		mustNewElement(tag.MediaStorageSOPInstanceUID, []string{"1.2.3.4.5.6.7"}),
		mustNewElement(tag.TransferSyntaxUID, []string{"1.2.840.10008.1.2.4.95"}),
		mustNewElement(tag.PatientName, []string{"ABCD"}),
	}}
	bo, implicit, err := ds.transferSyntax()
	if err != nil || bo != binary.LittleEndian || implicit {
		t.Errorf("transferSyntax() = %v, %v, %v; want little endian, explicit, nil", bo, implicit, err)
	}
	// As before, only Deflated Explicit VR Little Endian is refused; 4.95 is
	// written uncompressed.
	if err := Write(&bytes.Buffer{}, ds); err != nil {
		t.Errorf("Write error = %v", err)
	}
}

func TestParse_NonItemErrorOmitsTheValue(t *testing.T) {
	value := make([]byte, 1<<20)
	ob := explicitElem(privateBytesTag, "OB", uint32(len(value)), value)
	cases := map[string][]byte{
		"defined length":   explicitFile(explicitElem(tag.ContentSequence, "SQ", uint32(len(ob)), ob)),
		"undefined length": explicitFile(explicitElem(tag.ContentSequence, "SQ", tag.VLUndefinedLength, nil), ob, seqDelimitation),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseBounded(t, data, uint64(maxBoundedAlloc+2*len(value)))
			if err == nil {
				t.Fatal("Parse returned no error")
			}
			if len(err.Error()) > 1024 {
				t.Errorf("Parse error message is %d bytes, want at most 1024", len(err.Error()))
			}
		})
	}
}

func TestReadBytes_OWIsLittleEndianWhateverTheByteOrder(t *testing.T) {
	data := []byte{1, 2, 3, 4}
	r := &reader{rawReader: dicomio.NewReader(bufio.NewReader(bytes.NewReader(data)), binary.BigEndian, int64(len(data)))}
	v, err := r.readBytes(privateBytesTag, "OW", 4)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.GetValue().([]byte), []byte{2, 1, 4, 3}; !bytes.Equal(got, want) {
		t.Errorf("OW value = %v, want %v", got, want)
	}
}
