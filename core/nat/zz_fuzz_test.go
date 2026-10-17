package nat

import "testing"

func FuzzDecodeCandidates(f *testing.F) {
	f.Add([]byte{0})
	f.Add([]byte{1, 1, 4, 10, 0, 0, 1, 0x2a, 0x9a})
	f.Add([]byte{2, 2, 6})
	f.Fuzz(func(t *testing.T, b []byte) {
		// 不应 panic
		_, _ = DecodeCandidates(b)
	})
}

// FuzzEncodeDecodeRoundTrip 校验不变量：任意可成功解码的字节流，
// 重新编码后再解码，候选列表必须完全一致。
func FuzzEncodeDecodeRoundTrip(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0})
	f.Add([]byte{1, 1, 4, 10, 0, 0, 1, 0x2a, 0x9a})
	f.Fuzz(func(t *testing.T, b []byte) {
		first, err := DecodeCandidates(b)
		if err != nil {
			return
		}
		if len(first) > MaxCandidates {
			return
		}
		encoded := EncodeCandidates(first)
		second, err := DecodeCandidates(encoded)
		if err != nil {
			t.Fatalf("round-trip decode: %v", err)
		}
		if len(second) != len(first) {
			t.Fatalf("len: got %d want %d", len(second), len(first))
		}
		for i := range first {
			if second[i] != first[i] {
				t.Fatalf("[%d]: got %v want %v", i, second[i], first[i])
			}
		}
	})
}
