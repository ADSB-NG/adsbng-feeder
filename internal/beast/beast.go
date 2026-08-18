// Package beast implements Beast-protocol frame boundary detection.
//
// Scope, deliberately narrow: this package finds where frames start and end so
// the feeder can count them and avoid emitting partial frames. It does NOT
// decode Mode-S payloads, CPR positions, or anything else. All ADS-B decoding
// is ADSBNG-side and proprietary; none of it belongs here.
//
// Wire format (as produced by readsb/dump1090 --net-bo-port):
//
//	0x1a  <type>  <6-byte MLAT timestamp>  <1-byte signal>  <data...>
//
// Total UNESCAPED frame lengths by type:
//
//	0x31 '1' Mode-AC      11 bytes  (body 9  = 6 ts + 1 sig + 2  data)
//	0x32 '2' Mode-S short 16 bytes  (body 14 = 6 ts + 1 sig + 7  data)
//	0x33 '3' Mode-S long  23 bytes  (body 21 = 6 ts + 1 sig + 14 data)
//
// Escaping: a literal 0x1a inside the body is doubled on the wire (0x1a 0x1a).
// A single 0x1a is therefore always a frame start.
//
// Scan returns frames as their ORIGINAL still-escaped bytes. The feeder forwards
// those bytes verbatim, so forwarding is byte-exact and lossless — we never
// unescape and re-escape, which is the usual source of subtle corruption.
package beast

// Esc is the Beast escape/sync byte.
const Esc = 0x1a

// Frame type bytes.
const (
	TypeModeAC    = 0x31 // '1'
	TypeModeSShrt = 0x32 // '2'
	TypeModeSLong = 0x33 // '3'
)

// MaxFrameLen is the largest possible on-the-wire frame: a Mode-S long frame
// whose every body byte is 0x1a and therefore doubled — 2 + 21*2 = 44 bytes.
const MaxFrameLen = 44

// bodyLen returns the number of unescaped body bytes expected after the 0x1a
// sync byte and the type byte, or -1 if the type is not a known Beast type.
func bodyLen(typ byte) int {
	switch typ {
	case TypeModeAC:
		return 9
	case TypeModeSShrt:
		return 14
	case TypeModeSLong:
		return 21
	default:
		return -1
	}
}

// IsKnownType reports whether typ is a Beast frame type this build understands.
func IsKnownType(typ byte) bool { return bodyLen(typ) >= 0 }

// Scan extracts as many whole frames as possible from buf.
//
// It returns the frames (as sub-slices of buf, still escaped, in wire order),
// the number of leading bytes of buf that were consumed, and the number of bytes
// that were discarded as unparseable garbage.
//
// Callers keep buf[consumed:] as the carry-over for the next read. Sub-slices
// alias buf, so copy them if you retain them past the next append.
func Scan(buf []byte) (frames [][]byte, consumed, discarded int) {
	i := 0
	for {
		// Find the next sync byte, discarding anything before it.
		s := indexByte(buf[i:], Esc)
		if s < 0 {
			// No sync byte anywhere in the tail, so none of it can begin a frame:
			// discard all of it. Nothing needs to be carried over — a 0x1a that
			// arrives in a later read will be found by that read's scan.
			if n := len(buf) - i; n > 0 {
				discarded += n
				i = len(buf)
			}
			return frames, i, discarded
		}
		if s > 0 {
			discarded += s
			i += s
		}

		// Need at least the sync + type byte to know how long the frame is.
		if i+2 > len(buf) {
			return frames, i, discarded
		}

		n := bodyLen(buf[i+1])
		if n < 0 {
			// Unknown type: this 0x1a was not a real frame start. Skip just the
			// sync byte and resynchronise on the next one.
			discarded++
			i++
			continue
		}

		// Walk the body, counting UNESCAPED bytes. j indexes raw wire bytes.
		j := i + 2
		count := 0
		desync := false
		for count < n {
			if j >= len(buf) {
				// Incomplete frame — wait for more bytes.
				return frames, i, discarded
			}
			if buf[j] == Esc {
				if j+1 >= len(buf) {
					return frames, i, discarded // need the second byte to decide
				}
				if buf[j+1] != Esc {
					// A lone 0x1a inside the body means the stream desynced and
					// a new frame starts here. Abandon this frame.
					desync = true
					break
				}
				j += 2 // escaped literal 0x1a -> one body byte
			} else {
				j++
			}
			count++
		}
		if desync {
			discarded += j - i
			i = j
			continue
		}

		frames = append(frames, buf[i:j])
		i = j
	}
}

// indexByte is bytes.IndexByte, inlined to keep this package import-free.
func indexByte(b []byte, c byte) int {
	for i := 0; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}
