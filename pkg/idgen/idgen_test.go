package idgen_test

import (
	"sync"
	"testing"

	"github.com/april/shorten-link/pkg/idgen"
)

func TestNextID_Unique(t *testing.T) {
	g, err := idgen.NewGenerator(1)
	if err != nil {
		t.Fatal(err)
	}

	const n = 100_000
	ids := make(chan int64, n)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids <- g.NextID()
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[int64]struct{}, n)
	for id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate ID: %d", id)
		}
		seen[id] = struct{}{}
	}
}

func TestNextID_Ordered(t *testing.T) {
	g, err := idgen.NewGenerator(1)
	if err != nil {
		t.Fatal(err)
	}

	prev := g.NextID()
	for i := 0; i < 1000; i++ {
		next := g.NextID()
		if next <= prev {
			t.Fatalf("IDs not monotonically increasing: %d >= %d", prev, next)
		}
		prev = next
	}
}

func TestNewGenerator_InvalidNode(t *testing.T) {
	if _, err := idgen.NewGenerator(-1); err == nil {
		t.Error("expected error for nodeID -1")
	}
	if _, err := idgen.NewGenerator(1024); err == nil {
		t.Error("expected error for nodeID 1024")
	}
}

func TestToBase62(t *testing.T) {
	cases := []struct {
		in  int64
		out string
	}{
		{0, "0"},
		{62, "10"},
		{3844, "100"},
	}
	for _, c := range cases {
		got := idgen.ToBase62(c.in)
		if got != c.out {
			t.Errorf("ToBase62(%d) = %q, want %q", c.in, got, c.out)
		}
	}
}

func TestToBase62_SnowflakeLength(t *testing.T) {
	g, err := idgen.NewGenerator(1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		code := idgen.ToBase62(g.NextID())
		if l := len(code); l < 7 || l > 11 {
			t.Errorf("unexpected code length %d for code %q", l, code)
		}
	}
}
