package logring

import (
	"fmt"
	"reflect"
	"testing"
)

func TestRingKeepsTheNewestLinesInOrder(t *testing.T) {
	r := New(3)
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(r, "line %d\n", i)
	}
	want := []string{"line 3", "line 4", "line 5"}
	if got := r.Last(10); !reflect.DeepEqual(got, want) {
		t.Errorf("Last(10) = %v, want %v", got, want)
	}
	if got := r.Last(2); !reflect.DeepEqual(got, []string{"line 4", "line 5"}) {
		t.Errorf("Last(2) = %v", got)
	}
}

func TestRingJoinsLinesSplitAcrossWrites(t *testing.T) {
	r := New(5)
	r.Write([]byte("hel"))
	r.Write([]byte("lo wor"))
	r.Write([]byte("ld\nnext\r\n"))
	if got := r.Last(5); !reflect.DeepEqual(got, []string{"hello world", "next"}) {
		t.Errorf("got %v", got)
	}
	if got := New(2).Last(5); len(got) != 0 {
		t.Errorf("an empty ring returned %v", got)
	}
}
