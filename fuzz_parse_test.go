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
	// one-bit native pixels cost 65 (each bit an int, plus the frame
	// overhead readNativeFrames allows), and up to an eighth more for
	// allocator rounding. Elements cost at most about 22.
	fuzzAllocPerByte = 80
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

// inflatedSize is how many bytes the deflated dataset after data's file meta
// group inflates to, or 0 if data has no meta group length to find it by.
func inflatedSize(data []byte) uint64 {
	const lengthValue = 128 + 4 + 8 // preamble, magic word, group length header
	if len(data) < lengthValue+4 {
		return 0
	}
	start := uint64(lengthValue+4) + uint64(binary.LittleEndian.Uint32(data[lengthValue:]))
	if start > uint64(len(data)) {
		return 0
	}
	n, _ := io.Copy(io.Discard, flate.NewReader(bytes.NewReader(data[start:])))
	return uint64(n)
}
