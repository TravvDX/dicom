# Changes in this fork

## v1.1.1-travv.1 (from upstream v1.1.0)

Parsing malformed input is bounded: memory follows the bytes the input holds,
and malformed header values return errors instead of panicking.

### Parsing

- A declared value length larger than the bytes left is refused before
  anything is allocated, with an error wrapping
  `dicomio.ErrorInsufficientBytesLeft`. With no exact size (`ParseUntilEOF`,
  deflated input), values grow as their bytes arrive.
- Sequences nest at most `DefaultMaxSequenceDepth` (64) deep; deeper input
  returns `ErrorSequenceTooDeep`. The `MaxSequenceDepth(n)` option changes it.
- Native PixelData: dimensions are sized in 64 bits. Rows, Columns,
  BitsAllocated, SamplesPerPixel or NumberOfFrames of the wrong type or with
  no value return `ErrorInvalidPixelDimensions` (missing Rows, Columns,
  BitsAllocated or SamplesPerPixel return `ErrorElementNotFound`, as before)
  instead of panicking. Frames so small that the image would cost more than
  `max(10x the PixelData length (73x for 1 bit), 16 MiB)` return
  `ErrorNativeFramesTooSmall`. Under an exact size, truncated PixelData is
  refused before any frame is decoded or sent on the frame channel.
- SpecificCharacterSet written as bytes (VR UN or OB, as anonymizers do) is
  read as its CS string; bytes naming no known character set take the default
  in every mode.
- Transfer syntaxes: a uid under `1.2.840.10008.1.2.` missing from the uid
  list is read as explicit VR little endian, as listed compression syntaxes
  are. JPIP Referenced Deflate (`1.2.840.10008.1.2.4.95`) and JPIP HTJ2K
  Referenced Deflate (`1.2.840.10008.1.2.4.205`) now canonicalize to Deflated
  Explicit VR Little Endian and are inflated when parsed. `ParseTransferSyntaxUID`
  still returns little endian, explicit VR for them, and `Write` still writes
  them uncompressed, as v1.1.0 did, so a dataset written with one of them does
  not parse back.

### `pkg/dicomio`

- New: `Reader.Available`, `Reader.ExactLimit`, `Reader.ReadBytes`,
  `Reader.AppendBytes`.
- `ReadString` (and `ReadBytes`) refuse a length past the limit with an error
  that wraps `ErrorInsufficientBytesLeft`; compare it with `errors.Is`, not
  `==`.
- `PushLimit` refuses a negative limit, and its error for a limit past the
  current one now wraps `ErrorInsufficientBytesLeft`.
- `SetDeflate` keeps a limit: the bytes left times 1032, deflate's maximum
  expansion, instead of reading until EOF.
