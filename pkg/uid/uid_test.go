package uid

import (
	"encoding/binary"
	"testing"
)

func TestCanonicalTransferSyntaxUID(t *testing.T) {
	for in, want := range map[string]string{
		ImplicitVRLittleEndian:         ImplicitVRLittleEndian,
		ExplicitVRBigEndian:            ExplicitVRBigEndian,
		DeflatedExplicitVRLittleEndian: DeflatedExplicitVRLittleEndian,
		"1.2.840.10008.1.2.4.95":       DeflatedExplicitVRLittleEndian, // JPIP Referenced Deflate
		"1.2.840.10008.1.2.4.205":      DeflatedExplicitVRLittleEndian, // JPIP HTJ2K Referenced Deflate
		"1.2.840.10008.1.2.4.50":       ExplicitVRLittleEndian,         // JPEG Baseline, in the list
		"1.2.840.10008.1.2.5":          ExplicitVRLittleEndian,         // RLE Lossless, in the list
		"1.2.840.10008.1.2.4.94":       ExplicitVRLittleEndian,         // JPIP Referenced
		"1.2.840.10008.1.2.4.201":      ExplicitVRLittleEndian,         // HTJ2K, not in the list
		"1.2.840.10008.1.2.4.999":      ExplicitVRLittleEndian,         // a future syntax
	} {
		got, err := CanonicalTransferSyntaxUID(in)
		if err != nil || got != want {
			t.Errorf("CanonicalTransferSyntaxUID(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"1.2.3.4", "1.2.840.10008.1.1"} {
		if got, err := CanonicalTransferSyntaxUID(in); err == nil {
			t.Errorf("CanonicalTransferSyntaxUID(%q) = %q, want an error", in, got)
		}
	}
}

func TestParseTransferSyntaxUID_DeflatedDatasetSyntaxes(t *testing.T) {
	for _, in := range []string{"1.2.840.10008.1.2.4.95", "1.2.840.10008.1.2.4.205"} {
		bo, implicit, err := ParseTransferSyntaxUID(in)
		if err != nil || bo != binary.LittleEndian || implicit {
			t.Errorf("ParseTransferSyntaxUID(%q) = %v, %v, %v; want little endian, explicit, nil", in, bo, implicit, err)
		}
	}
}
