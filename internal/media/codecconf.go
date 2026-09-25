package media

import "fmt"

// Reading a codec's configuration record: the few bytes a container keeps
// beside a track so that a decoder can be set up before the first frame.
//
// Both containers carry the same records -- an MP4 in a box named for the
// record, a Matroska in the track's CodecPrivate -- so the two readers share
// these, and they answer in ffprobe's words because the two paths write the
// same columns (#207). What a record does not state is left at zero, which the
// row calls unknown: it is never worked out from something nearby.

// streamConf is what a configuration record says about a picture.
type streamConf struct {
	profile string
	level   int
	depth   int
}

// avcConf reads an AVCDecoderConfigurationRecord (ISO/IEC 14496-15, 5.3.3).
//
// The depth is in the record's extension, which only the profiles above High
// carry -- and only need to, since Baseline, Main, Extended and High are eight
// bits by definition. A High 10 record written without its extension leaves
// the depth unknown.
func avcConf(b []byte) (streamConf, bool) {
	if len(b) < 7 || b[0] != 1 {
		return streamConf{}, false
	}
	idc, constraints := b[1], b[2]
	c := streamConf{profile: avcProfile(idc, constraints), level: int(b[3])}

	switch idc {
	case 66, 77, 88, 100:
		c.depth = 8
	default:
		c.depth = avcExtensionDepth(b)
	}
	return c, true
}

// avcProfile is libavcodec's name for an H.264 profile. The constraint flags
// matter to two of them: Baseline with constraint_set1 is Constrained Baseline,
// and the profiles above High with constraint_set3 are their Intra variants.
func avcProfile(idc, constraints byte) string {
	set1 := constraints&0x40 != 0
	set3 := constraints&0x10 != 0
	switch idc {
	case 66:
		if set1 {
			return "Constrained Baseline"
		}
		return "Baseline"
	case 77:
		return "Main"
	case 88:
		return "Extended"
	case 100:
		return "High"
	case 110:
		if set3 {
			return "High 10 Intra"
		}
		return "High 10"
	case 122:
		if set3 {
			return "High 4:2:2 Intra"
		}
		return "High 4:2:2"
	case 244:
		if set3 {
			return "High 4:4:4 Intra"
		}
		return "High 4:4:4 Predictive"
	case 44:
		return "CAVLC 4:4:4"
	}
	return ""
}

// avcExtensionDepth steps over the parameter sets to the extension that
// follows them, and reads the luma depth out of it.
func avcExtensionDepth(b []byte) int {
	i := 5
	sps := int(b[i] & 0x1f)
	i++
	for range sps {
		if i+2 > len(b) {
			return 0
		}
		i += 2 + (int(b[i])<<8 | int(b[i+1]))
	}
	if i >= len(b) {
		return 0
	}
	pps := int(b[i])
	i++
	for range pps {
		if i+2 > len(b) {
			return 0
		}
		i += 2 + (int(b[i])<<8 | int(b[i+1]))
	}
	// chroma_format, then bit_depth_luma_minus8, each in the low bits of a
	// byte whose high bits are reserved ones.
	if i+2 > len(b) {
		return 0
	}
	return int(b[i+1]&0x07) + 8
}

// hevcConf reads an HEVCDecoderConfigurationRecord (ISO/IEC 14496-15, 8.3.3),
// which states its depth outright.
func hevcConf(b []byte) (streamConf, bool) {
	if len(b) < 23 || b[0] != 1 {
		return streamConf{}, false
	}
	return streamConf{
		profile: hevcProfile(b[1] & 0x1f),
		level:   int(b[12]),
		depth:   int(b[17]&0x07) + 8,
	}, true
}

func hevcProfile(idc byte) string {
	switch idc {
	case 1:
		return "Main"
	case 2:
		return "Main 10"
	case 3:
		return "Main Still Picture"
	case 4:
		return "Rext"
	case 9:
		return "SCC"
	}
	return ""
}

// vp9Conf reads a VPCodecConfigurationRecord, the payload of an MP4's vpcC
// after its version and flags.
//
// The level is not taken even when the record has one: ffprobe reports none
// for VP9, and a row must not say which reader produced it.
func vp9Conf(b []byte) (streamConf, bool) {
	if len(b) < 3 {
		return streamConf{}, false
	}
	return streamConf{profile: fmt.Sprintf("Profile %d", b[0]), depth: int(b[2] >> 4)}, true
}

// av1Conf reads an AV1CodecConfigurationRecord, whose first byte is a marker
// and a version, both fixed.
func av1Conf(b []byte) (streamConf, bool) {
	if len(b) < 4 || b[0] != 0x81 {
		return streamConf{}, false
	}
	c := streamConf{level: int(b[1] & 0x1f), depth: 8}
	switch b[1] >> 5 {
	case 0:
		c.profile = "Main"
	case 1:
		c.profile = "High"
	case 2:
		c.profile = "Professional"
	}
	highDepth, twelve := b[2]&0x40 != 0, b[2]&0x20 != 0
	switch {
	case highDepth && twelve:
		c.depth = 12
	case highDepth:
		c.depth = 10
	}
	return c, true
}

// aacChannels reads the channel configuration out of an AudioSpecificConfig
// (ISO/IEC 14496-3, 1.6.2.1). It is authoritative where the sample entry is
// not: an MP4 writer may put 2 in the entry's channel count for a 5.1 track.
// Zero means the channels are in a program config element this does not read.
func aacChannels(asc []byte) int {
	if len(asc) < 2 {
		return 0
	}
	bits := uint32(asc[0])<<24 | uint32(asc[1])<<16
	if len(asc) > 2 {
		bits |= uint32(asc[2]) << 8
	}
	if len(asc) > 3 {
		bits |= uint32(asc[3])
	}
	pos := 5
	if bits>>27 == 31 {
		pos += 6 // an escaped object type
	}
	if (bits>>(32-pos-4))&0x0f == 15 {
		pos += 24 // an explicit frequency rather than an index
	}
	pos += 4
	if pos+4 > 32 {
		return 0
	}
	switch conf := int((bits >> (32 - pos - 4)) & 0x0f); {
	case conf >= 1 && conf <= 6:
		return conf
	case conf == 7:
		return 8
	}
	return 0
}
