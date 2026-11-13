package exitcode

import "testing"

func TestWritesDocument(t *testing.T) {
	want := map[int]bool{Green: true, Yellow: true, Red: true, Config: false, Runtime: false, Blind: true}
	for code, w := range want {
		if got := WritesDocument(code); got != w {
			t.Errorf("WritesDocument(%d) = %v, want %v", code, got, w)
		}
	}
}
