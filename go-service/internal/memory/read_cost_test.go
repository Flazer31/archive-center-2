package memory

import (
	"reflect"
	"testing"
)

func TestStoredWordsStillFilterCurrentNames(t *testing.T) {
	k := Known{Memories: []KnownMemory{{ID: "M1", Memory: Memory{Kind: "event", Body: "한국어 단검을 찾았다"}}}}
	for _, name := range []string{"한국어", "단검", "찾았다", ""} {
		k.Names = []KnownName{{ID: "N1", Name: name}}
		k.Memories[0].BodyWords = nil
		want := Select(k, "한국어 단검을 찾았다", nil)
		k.Memories[0].BodyWords = Words(k.Memories[0].Body)
		got := Select(k, "한국어 단검을 찾았다", nil)
		for i := range got.Memories {
			got.Memories[i].BodyWords = nil
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("name=%q changed selection", name)
		}
	}
}
