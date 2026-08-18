package beast

import (
	"bytes"
	"math/rand"
	"testing"
)

// frame builds a wire-format Beast frame with the given type and unescaped body,
// doubling any 0x1a as the protocol requires.
func frame(typ byte, body []byte) []byte {
	out := []byte{Esc, typ}
	for _, b := range body {
		out = append(out, b)
		if b == Esc {
			out = append(out, Esc)
		}
	}
	return out
}

// bodyOf returns a body of the right length for typ, filled with a marker byte.
func bodyOf(typ, fill byte) []byte {
	n := bodyLen(typ)
	if n < 0 {
		panic("bodyOf: unknown type")
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = fill
	}
	return b
}

func TestBodyLenMatchesSpec(t *testing.T) {
	// Total frame length = 2 (sync+type) + body. The totals are fixed by the
	// Beast protocol; if these change, readsb compatibility is broken.
	for _, tc := range []struct {
		typ   byte
		total int
	}{
		{TypeModeAC, 11},
		{TypeModeSShrt, 16},
		{TypeModeSLong, 23},
	} {
		if got := 2 + bodyLen(tc.typ); got != tc.total {
			t.Errorf("type 0x%02x: total frame length = %d, want %d", tc.typ, got, tc.total)
		}
	}
	if bodyLen(0x99) != -1 {
		t.Error("bodyLen(0x99) should be -1 for an unknown type")
	}
	if IsKnownType(0x99) {
		t.Error("IsKnownType(0x99) = true, want false")
	}
	for _, typ := range []byte{TypeModeAC, TypeModeSShrt, TypeModeSLong} {
		if !IsKnownType(typ) {
			t.Errorf("IsKnownType(0x%02x) = false, want true", typ)
		}
	}
}

func TestScanSingleFrameEachType(t *testing.T) {
	for _, typ := range []byte{TypeModeAC, TypeModeSShrt, TypeModeSLong} {
		f := frame(typ, bodyOf(typ, 0x55))
		frames, consumed, discarded := Scan(f)

		if len(frames) != 1 {
			t.Fatalf("type 0x%02x: got %d frames, want 1", typ, len(frames))
		}
		if !bytes.Equal(frames[0], f) {
			t.Errorf("type 0x%02x: frame not returned byte-exact\n got %x\nwant %x", typ, frames[0], f)
		}
		if consumed != len(f) {
			t.Errorf("type 0x%02x: consumed %d, want %d", typ, consumed, len(f))
		}
		if discarded != 0 {
			t.Errorf("type 0x%02x: discarded %d, want 0", typ, discarded)
		}
	}
}

func TestScanMultipleFrames(t *testing.T) {
	a := frame(TypeModeSLong, bodyOf(TypeModeSLong, 0x01))
	b := frame(TypeModeSShrt, bodyOf(TypeModeSShrt, 0x02))
	c := frame(TypeModeAC, bodyOf(TypeModeAC, 0x03))
	in := concat(a, b, c)

	frames, consumed, discarded := Scan(in)
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
	}
	for i, want := range [][]byte{a, b, c} {
		if !bytes.Equal(frames[i], want) {
			t.Errorf("frame %d mismatch:\n got %x\nwant %x", i, frames[i], want)
		}
	}
	if consumed != len(in) || discarded != 0 {
		t.Errorf("consumed=%d discarded=%d, want %d and 0", consumed, discarded, len(in))
	}
}

// TestScanEscapedBodyIsForwardedVerbatim is the property the feeder depends on:
// escaped bytes are returned exactly as they arrived, never unescaped and
// re-escaped, so forwarding cannot corrupt a payload.
func TestScanEscapedBodyIsForwardedVerbatim(t *testing.T) {
	body := bodyOf(TypeModeSLong, 0x00)
	body[0] = Esc
	body[5] = Esc
	body[len(body)-1] = Esc
	f := frame(TypeModeSLong, body)

	// 21 body bytes, 3 of them escaped -> 2 + 21 + 3 = 26 wire bytes.
	if len(f) != 26 {
		t.Fatalf("test setup: wire length %d, want 26", len(f))
	}

	frames, consumed, discarded := Scan(f)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if !bytes.Equal(frames[0], f) {
		t.Errorf("escaped frame not byte-exact:\n got %x\nwant %x", frames[0], f)
	}
	if consumed != len(f) || discarded != 0 {
		t.Errorf("consumed=%d discarded=%d, want %d and 0", consumed, discarded, len(f))
	}
}

func TestScanMaxLengthFrame(t *testing.T) {
	// Every body byte is 0x1a, so every one is doubled: the worst case the
	// MaxFrameLen constant promises to cover.
	body := bodyOf(TypeModeSLong, Esc)
	f := frame(TypeModeSLong, body)
	if len(f) != MaxFrameLen {
		t.Fatalf("all-escape frame is %d bytes, but MaxFrameLen = %d", len(f), MaxFrameLen)
	}
	frames, consumed, _ := Scan(f)
	if len(frames) != 1 || consumed != len(f) {
		t.Fatalf("got %d frames consuming %d, want 1 consuming %d", len(frames), consumed, len(f))
	}
}

func TestScanPartialFrameIsNotConsumed(t *testing.T) {
	f := frame(TypeModeSLong, bodyOf(TypeModeSLong, 0x7f))
	for cut := 1; cut < len(f); cut++ {
		frames, consumed, discarded := Scan(f[:cut])
		if len(frames) != 0 {
			t.Errorf("cut at %d: got %d frames, want 0 (frame is incomplete)", cut, len(frames))
		}
		if consumed != 0 {
			t.Errorf("cut at %d: consumed %d, want 0 so the partial frame carries over", cut, consumed)
		}
		if discarded != 0 {
			t.Errorf("cut at %d: discarded %d, want 0", cut, discarded)
		}
	}
}

func TestScanLeadingGarbageIsDiscarded(t *testing.T) {
	junk := []byte{0x00, 0xff, 0x42, 0x7e}
	f := frame(TypeModeSShrt, bodyOf(TypeModeSShrt, 0x11))
	in := concat(junk, f)

	frames, consumed, discarded := Scan(in)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if !bytes.Equal(frames[0], f) {
		t.Error("frame after garbage was not recovered intact")
	}
	if discarded != len(junk) {
		t.Errorf("discarded %d, want %d", discarded, len(junk))
	}
	if consumed != len(in) {
		t.Errorf("consumed %d, want %d", consumed, len(in))
	}
}

func TestScanTrailingGarbageWithNoSyncIsDiscarded(t *testing.T) {
	f := frame(TypeModeAC, bodyOf(TypeModeAC, 0x22))
	junk := []byte{0x01, 0x02, 0x03}
	frames, consumed, discarded := Scan(concat(f, junk))

	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if discarded != len(junk) {
		t.Errorf("discarded %d, want %d", discarded, len(junk))
	}
	if consumed != len(f)+len(junk) {
		t.Errorf("consumed %d, want everything (%d)", consumed, len(f)+len(junk))
	}
}

func TestScanUnknownTypeResyncs(t *testing.T) {
	// 0x1a followed by a type byte we do not know: that 0x1a was not a real frame
	// start, so it is dropped and scanning resumes at the next 0x1a.
	good := frame(TypeModeSLong, bodyOf(TypeModeSLong, 0x33))
	in := concat([]byte{Esc, 0x99}, good)

	frames, consumed, discarded := Scan(in)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if !bytes.Equal(frames[0], good) {
		t.Error("did not resynchronise onto the following valid frame")
	}
	if discarded != 2 {
		t.Errorf("discarded %d, want 2 (the bogus sync byte and its type)", discarded)
	}
	if consumed != len(in) {
		t.Errorf("consumed %d, want %d", consumed, len(in))
	}
}

func TestScanDesyncOnLoneEscapeInBody(t *testing.T) {
	// A lone 0x1a inside a body cannot occur in a well-formed stream, so it means
	// a new frame starts there. The truncated frame is abandoned, not emitted.
	good := frame(TypeModeSShrt, bodyOf(TypeModeSShrt, 0x44))
	truncated := []byte{Esc, TypeModeSLong, 0x01, 0x02, 0x03} // claims 21 body bytes
	in := concat(truncated, good)

	frames, consumed, discarded := Scan(in)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1 (only the good one)", len(frames))
	}
	if !bytes.Equal(frames[0], good) {
		t.Errorf("wrong frame emitted:\n got %x\nwant %x", frames[0], good)
	}
	if discarded != len(truncated) {
		t.Errorf("discarded %d, want %d", discarded, len(truncated))
	}
	if consumed != len(in) {
		t.Errorf("consumed %d, want %d", consumed, len(in))
	}
}

// TestScanStreamingByteAtATime models the real read pattern: frames arrive split
// across arbitrary read boundaries and must be recovered exactly once each.
func TestScanStreamingByteAtATime(t *testing.T) {
	var want [][]byte
	var stream []byte
	types := []byte{TypeModeAC, TypeModeSShrt, TypeModeSLong}
	for i := 0; i < 30; i++ {
		typ := types[i%len(types)]
		body := bodyOf(typ, byte(i))
		if i%4 == 0 {
			body[0] = Esc // exercise escaping across boundaries too
		}
		f := frame(typ, body)
		want = append(want, f)
		stream = append(stream, f...)
	}

	var (
		carry []byte
		got   [][]byte
	)
	for i := 0; i < len(stream); i++ {
		carry = append(carry, stream[i])
		frames, consumed, discarded := Scan(carry)
		if discarded != 0 {
			t.Fatalf("at byte %d: discarded %d bytes of a well-formed stream", i, discarded)
		}
		for _, f := range frames {
			cp := make([]byte, len(f))
			copy(cp, f)
			got = append(got, cp)
		}
		carry = append(carry[:0], carry[consumed:]...)
	}

	if len(carry) != 0 {
		t.Errorf("%d bytes left over after a whole-frame stream: %x", len(carry), carry)
	}
	if len(got) != len(want) {
		t.Fatalf("recovered %d frames, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("frame %d differs after streaming:\n got %x\nwant %x", i, got[i], want[i])
		}
	}
}

// TestScanChunkedRandomBoundaries is the same guarantee under random split sizes.
func TestScanChunkedRandomBoundaries(t *testing.T) {
	rng := rand.New(rand.NewSource(7))

	var want [][]byte
	var stream []byte
	for i := 0; i < 200; i++ {
		typ := []byte{TypeModeAC, TypeModeSShrt, TypeModeSLong}[rng.Intn(3)]
		body := bodyOf(typ, byte(rng.Intn(256)))
		for j := range body {
			if rng.Intn(8) == 0 {
				body[j] = Esc
			}
		}
		f := frame(typ, body)
		want = append(want, f)
		stream = append(stream, f...)
	}

	var (
		carry []byte
		got   [][]byte
		pos   int
	)
	for pos < len(stream) {
		n := 1 + rng.Intn(64)
		if pos+n > len(stream) {
			n = len(stream) - pos
		}
		carry = append(carry, stream[pos:pos+n]...)
		pos += n

		frames, consumed, discarded := Scan(carry)
		if discarded != 0 {
			t.Fatalf("discarded %d bytes of a well-formed stream", discarded)
		}
		for _, f := range frames {
			cp := make([]byte, len(f))
			copy(cp, f)
			got = append(got, cp)
		}
		carry = append(carry[:0], carry[consumed:]...)
	}

	if len(got) != len(want) {
		t.Fatalf("recovered %d frames, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("frame %d differs:\n got %x\nwant %x", i, got[i], want[i])
		}
	}
}

func TestScanEmptyInput(t *testing.T) {
	frames, consumed, discarded := Scan(nil)
	if len(frames) != 0 || consumed != 0 || discarded != 0 {
		t.Errorf("Scan(nil) = %d frames, consumed %d, discarded %d; want 0,0,0",
			len(frames), consumed, discarded)
	}
	frames, consumed, discarded = Scan([]byte{})
	if len(frames) != 0 || consumed != 0 || discarded != 0 {
		t.Errorf("Scan(empty) = %d frames, consumed %d, discarded %d; want 0,0,0",
			len(frames), consumed, discarded)
	}
}

func TestScanLoneSyncByteWaitsForType(t *testing.T) {
	frames, consumed, discarded := Scan([]byte{Esc})
	if len(frames) != 0 {
		t.Errorf("got %d frames from a lone sync byte, want 0", len(frames))
	}
	if consumed != 0 {
		t.Errorf("consumed %d, want 0 so the sync byte carries over to the next read", consumed)
	}
	if discarded != 0 {
		t.Errorf("discarded %d, want 0", discarded)
	}
}

// TestScanNeverPanicsOnArbitraryInput matters because Scan runs on bytes from an
// untrusted feeder: a panic here would be a remote denial of service.
func TestScanNeverPanicsOnArbitraryInput(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	for i := 0; i < 4000; i++ {
		buf := make([]byte, rng.Intn(128))
		for j := range buf {
			// Bias towards 0x1a so escape/desync paths are hit often.
			if rng.Intn(3) == 0 {
				buf[j] = Esc
			} else {
				buf[j] = byte(rng.Intn(256))
			}
		}
		frames, consumed, discarded := Scan(buf)

		if consumed < 0 || consumed > len(buf) {
			t.Fatalf("consumed %d out of range for a %d-byte buffer", consumed, len(buf))
		}
		if discarded < 0 || discarded > len(buf) {
			t.Fatalf("discarded %d out of range for a %d-byte buffer", discarded, len(buf))
		}
		// Every emitted frame must be a real frame: correct sync byte, known
		// type, and no longer than the protocol allows.
		for _, f := range frames {
			if len(f) < 2 || len(f) > MaxFrameLen {
				t.Fatalf("emitted a %d-byte frame from garbage: %x", len(f), f)
			}
			if f[0] != Esc {
				t.Fatalf("emitted a frame not starting with the sync byte: %x", f)
			}
			if !IsKnownType(f[1]) {
				t.Fatalf("emitted a frame with unknown type 0x%02x: %x", f[1], f)
			}
		}
	}
}

// TestScanAccountsForEveryConsumedByte checks the invariant the callers rely on
// for their counters: consumed bytes are either inside an emitted frame or
// counted as discarded. Anything else would mean silent data loss.
func TestScanAccountsForEveryConsumedByte(t *testing.T) {
	rng := rand.New(rand.NewSource(1234))
	for i := 0; i < 2000; i++ {
		buf := make([]byte, rng.Intn(200))
		for j := range buf {
			if rng.Intn(4) == 0 {
				buf[j] = Esc
			} else {
				buf[j] = byte(rng.Intn(256))
			}
		}
		frames, consumed, discarded := Scan(buf)

		inFrames := 0
		for _, f := range frames {
			inFrames += len(f)
		}
		if inFrames+discarded != consumed {
			t.Fatalf("accounting broken: %d bytes in frames + %d discarded != %d consumed\ninput %x",
				inFrames, discarded, consumed, buf)
		}
	}
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
