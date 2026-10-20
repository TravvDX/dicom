package uid

import (
	"encoding/binary"
	"fmt"
)

// explicitVRLittleEndianSyntaxes are standard transfer syntaxes defined after
// this package's list was generated whose dataset is plain explicit VR little
// endian; only their pixel data is compressed or referenced.
var explicitVRLittleEndianSyntaxes = map[string]bool{
	"1.2.840.10008.1.2.1.98":    true, // Encapsulated Uncompressed Explicit VR Little Endian
	"1.2.840.10008.1.2.4.100.1": true, // MPEG2 Main Profile / Main Level (fragmentable)
	"1.2.840.10008.1.2.4.101.1": true, // MPEG2 Main Profile / High Level (fragmentable)
	"1.2.840.10008.1.2.4.102.1": true, // MPEG-4 AVC/H.264 High Profile / Level 4.1 (fragmentable)
	"1.2.840.10008.1.2.4.103.1": true, // MPEG-4 AVC/H.264 BD-compatible High Profile / Level 4.1 (fragmentable)
	"1.2.840.10008.1.2.4.104.1": true, // MPEG-4 AVC/H.264 High Profile / Level 4.2 For 2D Video (fragmentable)
	"1.2.840.10008.1.2.4.105.1": true, // MPEG-4 AVC/H.264 High Profile / Level 4.2 For 3D Video (fragmentable)
	"1.2.840.10008.1.2.4.106.1": true, // MPEG-4 AVC/H.264 Stereo High Profile / Level 4.2 (fragmentable)
	"1.2.840.10008.1.2.4.110":   true, // JPEG XL Lossless
	"1.2.840.10008.1.2.4.111":   true, // JPEG XL JPEG Recompression
	"1.2.840.10008.1.2.4.112":   true, // JPEG XL
	"1.2.840.10008.1.2.4.201":   true, // High-Throughput JPEG 2000 Image Compression (Lossless Only)
	"1.2.840.10008.1.2.4.202":   true, // High-Throughput JPEG 2000 with RPCL Options Image Compression (Lossless Only)
	"1.2.840.10008.1.2.4.203":   true, // High-Throughput JPEG 2000 Image Compression
	"1.2.840.10008.1.2.4.204":   true, // JPIP HTJ2K Referenced
	"1.2.840.10008.1.2.7.1":     true, // SMPTE ST 2110-20 Uncompressed Progressive Active Video
	"1.2.840.10008.1.2.7.2":     true, // SMPTE ST 2110-20 Uncompressed Interlaced Active Video
	"1.2.840.10008.1.2.7.3":     true, // SMPTE ST 2110-30 PCM Digital Audio
	"1.2.840.10008.1.2.8.1":     true, // Deflated Image Frame Compression (frames deflated, not the dataset)
}

// deflatedDatasetSyntaxes deflate the whole dataset after the file meta
// group, which the parser inflates only for DeflatedExplicitVRLittleEndian.
// Read as explicit VR little endian, their compressed bytes would be parsed
// as elements.
var deflatedDatasetSyntaxes = map[string]bool{
	"1.2.840.10008.1.2.4.95":  true, // JPIP Referenced Deflate
	"1.2.840.10008.1.2.4.205": true, // JPIP HTJ2K Referenced Deflate
}

// Standard list of transfer syntaxes.
var StandardTransferSyntaxes = []string{
	ImplicitVRLittleEndian,
	ExplicitVRLittleEndian,
	ExplicitVRBigEndian,
	DeflatedExplicitVRLittleEndian,
}

// CanonicalTransferSyntaxUID return the canonical transfer syntax UID (e.g.,
// dicomuid.ExplicitVRLittleEndian or dicomuid.ImplicitVRLittleEndian), given an
// UID that represents any transfer syntax.  Returns an error if the uid is not
// defined in DICOM standard, if the uid does not represent a transfer syntax,
// or if it deflates the dataset in a way this package cannot read.
func CanonicalTransferSyntaxUID(uid string) (string, error) {
	// defaults are explicit VR, little endian
	switch uid {
	case ImplicitVRLittleEndian,
		ExplicitVRLittleEndian,
		ExplicitVRBigEndian,
		DeflatedExplicitVRLittleEndian:
		return uid, nil
	default:
		if deflatedDatasetSyntaxes[uid] {
			return "", fmt.Errorf("dicom.CanonicalTransferSyntaxUID: '%s' deflates the dataset, which is only supported for %s", uid, DeflatedExplicitVRLittleEndian)
		}
		if explicitVRLittleEndianSyntaxes[uid] {
			return ExplicitVRLittleEndian, nil
		}
		e, err := Lookup(uid)
		if err != nil {
			return "", err
		}
		if e.Type != TypeTransferSyntax {
			return "", fmt.Errorf("dicom.CanonicalTransferSyntaxUID: '%s' is not a transfer syntax (is %s)", uid, e.Type)
		}
		// The default is ExplicitVRLittleEndian
		return ExplicitVRLittleEndian, nil
	}
}

// ParseTransferSyntaxUID parses a transfer syntax uid and returns its byteorder
// and implicitVR/explicitVR type.  TransferSyntaxUID can be any UID that refers to
// a transfer syntax. It can be, e.g., 1.2.840.10008.1.2 (it will return
// LittleEndian, ImplicitVR) or 1.2.840.10008.1.2.4.54 (it will return
// (LittleEndian, ExplicitVR).
func ParseTransferSyntaxUID(uid string) (bo binary.ByteOrder, implicit bool, err error) {
	canonical, err := CanonicalTransferSyntaxUID(uid)
	if err != nil {
		return nil, false, err
	}
	switch canonical {
	case ImplicitVRLittleEndian:
		return binary.LittleEndian, true, nil
	case DeflatedExplicitVRLittleEndian:
		return binary.LittleEndian, false, nil
	case ExplicitVRLittleEndian:
		return binary.LittleEndian, false, nil
	case ExplicitVRBigEndian:
		return binary.BigEndian, false, nil
	default:
		return binary.BigEndian, false, fmt.Errorf("invalid or unknown transfer syntax: %v,  %v",
			canonical, uid)
	}
}
