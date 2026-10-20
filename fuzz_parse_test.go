package dicom

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
)

// vendorSeedsEnv names a directory of extra .dcm seeds for FuzzParse. The
// vendor files it is meant for carry no license that allows committing them.
const vendorSeedsEnv = "DICOM_VENDOR_SEEDS"

const (
	// fuzzAllocBase covers the fixed cost of a parse.
	fuzzAllocBase = 1 << 20
	// fuzzAllocPerByte is the most a byte of input may cost once parsed:
	// one-bit native pixels cost up to nativeAllocMultiple(1) (73), plus up to
	// an eighth more for allocator rounding. Elements cost at most about 22.
	fuzzAllocPerByte = 84
)

func FuzzParse(f *testing.F) {
	for _, tc := range boundsCases {
		f.Add(tc.data)
	}
	f.Add(explicitFile(nestedUndefined(DefaultMaxSequenceDepth, "SQ")))
	f.Add(explicitFile(nestedDefined(3)))
	f.Add(explicitFile(nativeImage(2, 4, 1, 1, "2 "), explicitElem(tag.PixelData, "OW", 2, []byte{0xaa, 0x55})))
	f.Add(explicitFile(nativeImage(2, 2, 16, 1, "1 "), explicitElem(tag.PixelData, "OW", 8, make([]byte, 8))))
	f.Add(deflatedFile(explicitElem(tag.PatientName, "PN", 4, []byte("ABCD"))))
	for _, tc := range untrustedLimitCases {
		f.Add(deflatedFile(tc.body))
	}
	f.Add(explicitFile(nativeImage(1, 1, 8, 1, "4096"), explicitElem(tag.PixelData, "OW", 4096, make([]byte, 4096))))
	f.Add(withoutGroupLength(deflatedFile(nativeImage(1024, 1024, 1, 1, "1 "), explicitElem(tag.PixelData, "OW", 1024*1024/8, make([]byte, 1024*1024/8)))))
	f.Add(withUSGroupLength(deflatedFile(nativeImage(1024, 1024, 1, 1, "1 "), explicitElem(tag.PixelData, "OW", 1024*1024/8, make([]byte, 1024*1024/8)))))
	for _, name := range []string{"1.dcm", "2.dcm", "3.dcm", "6.dcm"} {
		addSeedFile(f, filepath.Join("testdata", name))
	}
	if dir := os.Getenv(vendorSeedsEnv); dir != "" {
		paths, err := filepath.Glob(filepath.Join(dir, "*.dcm"))
		if err != nil || len(paths) == 0 {
			f.Fatalf("%s=%s holds no .dcm files (err=%v)", vendorSeedsEnv, dir, err)
		}
		for _, p := range paths {
			addSeedFile(f, p)
		}
	}

	optionSets := [][]ParseOption{
		nil,
		{SkipPixelData()},
		{SkipProcessingPixelDataValue()},
		{AllowMismatchPixelDataLength(), AllowMissingMetaElementGroupLength(), AllowUnknownSpecificCharacterSet()},
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		// A deflated input is bounded by what it inflates to, not by the most
		// it could.
		read := uint64(len(data))
		if bytes.Contains(data, []byte(uid.DeflatedExplicitVRLittleEndian)) {
			read += inflatedSize(data)
		}
		limit := fuzzAllocBase + fuzzAllocPerByte*read
		allocated := bytesAllocated(func() {
			_, _ = ParseUntilEOF(bytes.NewReader(data), nil)
		})
		if allocated > limit {
			t.Fatalf("ParseUntilEOF of a %d byte input (%d read) allocated %d bytes, want at most %d", len(data), read, allocated, limit)
		}
		for _, opts := range optionSets {
			allocated := bytesAllocated(func() {
				_, _ = Parse(bytes.NewReader(data), int64(len(data)), nil, opts...)
			})
			if allocated > limit {
				t.Fatalf("Parse of a %d byte input (%d read) allocated %d bytes, want at most %d", len(data), read, allocated, limit)
			}
		}
	})
}

func addSeedFile(f *testing.F, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(data)
}

// withUSGroupLength rewrites dicomFile's UL group length as a US, which the
// parser also accepts.
func withUSGroupLength(data []byte) []byte {
	const at = 128 + 4
	length := binary.LittleEndian.Uint32(data[at+8:])
	return cat(data[:at], explicitElem(tag.FileMetaInformationGroupLength, "US", 2, us(uint16(length))), data[at+12:])
}

// withoutGroupLength puts another group's element where dicomFile's group
// length was, so that the parser, with AllowMissingMetaElementGroupLength,
// reads it and walks the meta group after it.
func withoutGroupLength(data []byte) []byte {
	const at = 128 + 4
	return cat(data[:at], explicitElem(tag.Tag{Group: 0x0077, Element: 0x0000}, "CS", 2, []byte("AB")), data[at+12:])
}

// inflatedSize is the most the dataset after data's file meta group
// inflates to, trying both ends the parser may take for that group: the one
// its group length gives, and the one walking its group 0002 elements finds.
func inflatedSize(data []byte) uint64 {
	const metaStart = 128 + 4 // preamble, magic word
	var most uint64
	for _, start := range []int{groupLengthEnd(data, metaStart), metaWalkEnd(data, metaStart)} {
		if start <= 0 || start > len(data) {
			continue
		}
		n, _ := io.Copy(io.Discard, flate.NewReader(bytes.NewReader(data[start:])))
		most = max(most, uint64(n))
	}
	return most
}

// groupLengthEnd is where the meta group ends by its (0002,0000) value, or 0
// where the parser takes no group length. As readHeader does, it takes the
// first value of any integer VR, whose values must fill the element exactly.
func groupLengthEnd(data []byte, at int) int {
	if len(data) < at+8 || binary.LittleEndian.Uint16(data[at:]) != 0x0002 || binary.LittleEndian.Uint16(data[at+2:]) != 0 {
		return 0
	}
	vr := string(data[at+4 : at+6])
	vl := int(binary.LittleEndian.Uint16(data[at+6:]))
	value := at + 8
	end := value + vl
	if len(data) < end {
		return 0
	}
	size := 2
	if vr == "UL" || vr == "SL" {
		size = 4
	}
	if vl == 0 || vl%size != 0 {
		return 0
	}
	var length int64
	switch vr {
	case "UL":
		length = int64(binary.LittleEndian.Uint32(data[value:]))
	case "SL":
		length = int64(int32(binary.LittleEndian.Uint32(data[value:])))
	case "US", "AT":
		length = int64(binary.LittleEndian.Uint16(data[value:]))
	case "SS":
		length = int64(int16(binary.LittleEndian.Uint16(data[value:])))
	default:
		return 0
	}
	// A negative length leaves the limit behind the reader, which ends the
	// group at once.
	return end + int(max(length, 0))
}

// metaWalkEnd is where the meta group ends when the parser walks it: past the
// first element, whatever its group, then past each group 0002 element.
func metaWalkEnd(data []byte, at int) int {
	for first := true; len(data) >= at+8 && (first || binary.LittleEndian.Uint16(data[at:]) == 0x0002); first = false {
		vr := string(data[at+4 : at+6])
		if longVRs[vr] {
			if len(data) < at+12 {
				return 0
			}
			at += 12 + int(binary.LittleEndian.Uint32(data[at+8:]))
		} else {
			at += 8 + int(binary.LittleEndian.Uint16(data[at+6:]))
		}
		if at < 0 {
			return 0
		}
	}
	return at
}
