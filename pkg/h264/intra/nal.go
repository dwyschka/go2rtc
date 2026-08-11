package intra

// NAL unit types we care about.
const (
	nalSliceIDR = 5
	nalSPS      = 7
	nalPPS      = 8
)

// nalUnit is one parsed NAL: its type and de-emulated RBSP payload (without the
// 1-byte NAL header).
type nalUnit struct {
	typ  byte
	rbsp []byte
}

// splitAnnexB splits an Annex-B bitstream (start-code delimited) into NAL units,
// stripping start codes and emulation-prevention bytes. Also accepts a stream
// with a single NAL and no leading start code.
func splitAnnexB(b []byte) []nalUnit {
	var out []nalUnit
	starts := startCodeOffsets(b)
	if len(starts) == 0 {
		return out
	}
	for i, s := range starts {
		end := len(b)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		nal := trimStartCode(b[s:end])
		if len(nal) < 1 {
			continue
		}
		out = append(out, nalUnit{
			typ:  nal[0] & 0x1f,
			rbsp: removeEmulation(nal[1:]),
		})
	}
	return out
}

// startCodeOffsets returns the byte offset of each start code (00 00 01 or
// 00 00 00 01) in b.
func startCodeOffsets(b []byte) []int {
	var offs []int
	for i := 0; i+3 <= len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			offs = append(offs, i)
			i += 2
		}
	}
	return offs
}

// trimStartCode drops a leading 00 00 01 / 00 00 00 01 prefix from a NAL slice.
func trimStartCode(nal []byte) []byte {
	if len(nal) >= 3 && nal[0] == 0 && nal[1] == 0 && nal[2] == 1 {
		return nal[3:]
	}
	if len(nal) >= 4 && nal[0] == 0 && nal[1] == 0 && nal[2] == 0 && nal[3] == 1 {
		return nal[4:]
	}
	return nal
}

// removeEmulation strips emulation-prevention bytes: every 0x03 in a
// 00 00 03 xx (xx <= 0x03) sequence is removed, yielding the RBSP.
func removeEmulation(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for i := 0; i < len(b); i++ {
		if zeros >= 2 && b[i] == 3 {
			// 00 00 03 -> the 03 is the emulation-prevention byte; drop it and
			// reset the run so a following 00 00 is handled fresh.
			zeros = 0
			continue
		}
		out = append(out, b[i])
		if b[i] == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}
