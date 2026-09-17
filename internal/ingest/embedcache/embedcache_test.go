package embedcache

import (
	"encoding/hex"
	"reflect"
	"testing"
)

func TestKeyByHexAndEmptyInput(t *testing.T) {
	// keyByHex maps parallel hashes+vectors to hex-keyed map.
	h1 := []byte{0xab, 0xcd}
	got := keyByHex([][]byte{h1}, [][]float32{{1, 2}})
	want := map[string][]float32{hex.EncodeToString(h1): {1, 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keyByHex = %v, want %v", got, want)
	}
	if len(keyByHex(nil, nil)) != 0 {
		t.Fatal("empty input must yield empty map")
	}
}
