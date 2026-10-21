package webhook

import (
	"bytes"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func FuzzVerifyNeverPanics(f *testing.F) {
	f.Add(specID, strconv.Itoa(specTS), specSigning, []byte(specBody))
	f.Add("", "", "", []byte(nil))
	f.Add("id", "-1", "v1,", []byte("x"))
	f.Add("id", "99999999999999999999", "v1,AAAA v1,BBBB v2,CC", []byte("x"))
	f.Add("id", strconv.Itoa(specTS), "v1,"+string(bytes.Repeat([]byte("A"), 1<<12)), []byte("x"))
	v, _ := NewVerifier(specSecret)
	v.Now = func() time.Time { return time.Unix(specTS, 0) }
	f.Fuzz(func(t *testing.T, id, ts, sig string, body []byte) {
		h := http.Header{}
		h.Set(HeaderID, id)
		h.Set(HeaderTimestamp, ts)
		h.Set(HeaderSignature, sig)
		err := v.Verify(h, body) // must return, whatever the headers hold
		if err == nil {
			// Accepting means a valid signature for exactly these inputs.
			key, _ := DecodeSecret(specSecret)
			unix, perr := strconv.ParseInt(ts, 10, 64)
			if perr != nil {
				t.Fatalf("accepted a timestamp that is not an integer: %q", ts)
			}
			want := Sign(key, id, time.Unix(unix, 0), body)
			found := false
			for _, s := range bytes.Fields([]byte(sig)) {
				if string(s) == want {
					found = true
				}
			}
			if !found {
				t.Fatalf("accepted without the correct signature %q in %q", want, sig)
			}
		}
	})
}

func FuzzSignVerifyRoundTrip(f *testing.F) {
	f.Add("msg_1", int64(0), []byte(`{"a":1}`), uint(0))
	f.Add("a.b.c", int64(-299), []byte(""), uint(3))
	f.Fuzz(func(t *testing.T, id string, offsetSeconds int64, body []byte, flip uint) {
		if id == "" || offsetSeconds < -299 || offsetSeconds > 299 {
			return // outside the tolerance window, or no id: covered by unit tests
		}
		now := time.Unix(1_800_000_000, 0)
		ts := now.Add(time.Duration(offsetSeconds) * time.Second)
		v, _ := NewVerifier(specSecret)
		v.Now = func() time.Time { return now }
		key, _ := DecodeSecret(specSecret)

		h := http.Header{}
		h.Set(HeaderID, id)
		h.Set(HeaderTimestamp, strconv.FormatInt(ts.Unix(), 10))
		h.Set(HeaderSignature, Sign(key, id, ts, body))
		if err := v.Verify(h, body); err != nil {
			t.Fatalf("a request signed by us does not verify: %v", err)
		}
		if len(body) > 0 {
			tampered := bytes.Clone(body)
			tampered[int(flip)%len(tampered)] ^= 0x01
			if err := v.Verify(h, tampered); err == nil {
				t.Fatalf("a body with one bit flipped (byte %d) still verifies", int(flip)%len(tampered))
			}
		}
	})
}

func FuzzDecodeSecret(f *testing.F) {
	for _, s := range []string{specSecret, "whsec_", "whsec_====", "whsec_-_8=", "", "whsec_\x00", " whsec_aGk= "} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		key, err := DecodeSecret(s)
		if err == nil && len(key) == 0 {
			t.Fatalf("DecodeSecret(%q) accepted an empty key", s)
		}
		if err != nil && key != nil {
			t.Fatalf("DecodeSecret(%q) returned both a key and an error", s)
		}
	})
}
