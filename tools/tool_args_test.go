package tools

import (
	"reflect"
	"testing"
)

func TestParseFindToolArgsToleratesLooseShapes(t *testing.T) {
	args, err := RepairAndUnmarshalArgs(`[{"names":"manage_data","limit":"7",}]`)
	if err != nil {
		t.Fatalf("a single-element array with a trailing comma should repair: %v", err)
	}
	got := ParseFindToolArgs(args)
	want := FindToolArgs{Names: []string{"manage_data"}, Limit: 7}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}

	args, _ = RepairAndUnmarshalArgs(`{"names":[" a ","","b"],"query":"q","limit":3}`)
	got = ParseFindToolArgs(args)
	want = FindToolArgs{Names: []string{"a", "b"}, Query: "q", Limit: 3}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
