package rdb

import "testing"

// Expected values were measured on Redis 7.4 with:
//
//	SET zz <n bytes>; MEMORY USAGE zz SAMPLES 0
//
// MEMORY USAGE excludes the main-dict bucket pointer, so we subtract 8.
func TestStringMemoryMatchesRedis(t *testing.T) {
	measured := map[uint64]int64{
		1: 56, 5: 64, 13: 72, 20: 72, 21: 80, 28: 80, 29: 88, 36: 88,
		37: 96, 44: 96, 45: 104, 60: 112, 100: 160, 130: 208, 200: 272,
	}
	for n, want := range measured {
		a := memAcc{strLen: n}
		got := keyOverhead(len("zz"), false) - 8 + valueMem(TypeString, &a, 0)
		if got != want {
			t.Errorf("len %d: estimate %d, redis %d", n, got, want)
		}
	}
}

func TestJemallocSize(t *testing.T) {
	tests := map[uint64]uint64{
		1: 8, 8: 8, 9: 16, 20: 24, 40: 40, 64: 64, 65: 80, 128: 128,
		129: 160, 200: 224, 257: 320, 1000: 1024, 50020: 57344,
	}
	for in, want := range tests {
		if got := jemallocSize(in); got != want {
			t.Errorf("jemallocSize(%d) = %d, want %d", in, got, want)
		}
	}
}
