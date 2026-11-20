package clock

import (
	"testing"
	"time"
)

func TestShifted(t *testing.T) {
	start := time.Date(2026, 4, 6, 4, 30, 0, 0, time.UTC)
	c := NewShifted(start)
	a := c.Now()
	if d := a.Sub(start); d < 0 || d > time.Second {
		t.Fatalf("Now = %s, want about %s", a, start)
	}
	time.Sleep(2 * time.Millisecond)
	if b := c.Now(); !b.After(a) {
		t.Fatalf("clock did not run: %s then %s", a, b)
	}
	later := start.Add(240 * time.Hour)
	c.Set(later)
	if d := c.Now().Sub(later); d < 0 || d > time.Second {
		t.Fatalf("after Set, Now = %s", c.Now())
	}
	c.Set(start) // backwards is ignored
	if c.Now().Before(later) {
		t.Fatal("clock moved backwards")
	}
}

func TestFake(t *testing.T) {
	f := NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	f.Advance(time.Hour)
	if f.Now().Hour() != 1 {
		t.Fatal(f.Now())
	}
}
