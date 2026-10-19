package dicom

import (
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"runtime"
	"testing"

	"github.com/suyashkumar/dicom/pkg/dicomio"
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
	var b bytes.Buffer
	w, _ := flate.NewWriter(&b, flate.BestCompression)
	w.Write(cat(body...))
	w.Close()
	return dicomFile(uid.DeflatedExplicitVRLittleEndian, b.Bytes())
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
	data := explicitFile(
		explicitElem(tag.SpecificCharacterSet, "US", 2, us(1)),
		explicitElem(tag.PatientName, "PN", 4, []byte("ABCD")),
	)
	if _, err := parseBounded(t, data, maxBoundedAlloc); err != nil {
		t.Errorf("Parse error = %v, want the default character set", err)
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
