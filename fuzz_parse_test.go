package dicom

import (
	"bytes"
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
	// fuzzAllocPerByte is the most a byte of input may cost once parsed. The
	// worst case measured is about 185: one-byte native frames of one-bit
	// pixels, each byte becoming a Frame, a NativeFrame and eight ints.
	fuzzAllocPerByte = 256
	// maxDeflateRatio mirrors the bound in dicomio.Reader.SetDeflate.
	maxDeflateRatio = 1032
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
		for _, opts := range optionSets {
			var ds Dataset
			allocated := bytesAllocated(func() {
				ds, _ = Parse(bytes.NewReader(data), int64(len(data)), nil, opts...)
			})
			inflated := uint64(len(data))
			if isDeflated(ds) {
				inflated *= maxDeflateRatio
			}
			if limit := fuzzAllocBase + fuzzAllocPerByte*inflated; allocated > limit {
				t.Fatalf("Parse of a %d byte input allocated %d bytes, want at most %d", len(data), allocated, limit)
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

func isDeflated(ds Dataset) bool {
	e, err := ds.FindElementByTag(tag.TransferSyntaxUID)
	if err != nil {
		return false
	}
	v, ok := e.Value.GetValue().([]string)
	return ok && len(v) > 0 && v[0] == uid.DeflatedExplicitVRLittleEndian
}
