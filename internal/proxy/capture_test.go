package proxy

import "testing"

func TestCapBuffer_AccumulatesUnderCap(t *testing.T) {
	c := newCapBuffer(100)
	c.Write([]byte("hello "))
	c.Write([]byte("world"))

	if got := string(c.Bytes()); got != "hello world" {
		t.Errorf("Bytes() = %q, want %q", got, "hello world")
	}
	if c.truncated {
		t.Error("truncated = true, want false")
	}
}

func TestCapBuffer_TruncatesAtCap(t *testing.T) {
	c := newCapBuffer(10)
	c.Write([]byte("0123456789")) // exactly at cap
	if c.truncated {
		t.Fatal("truncated = true after exactly filling the cap, want false")
	}

	c.Write([]byte("X")) // now over cap
	if !c.truncated {
		t.Fatal("truncated = false after exceeding the cap, want true")
	}
	if got := string(c.Bytes()); got != "0123456789" {
		t.Errorf("Bytes() = %q, want unchanged %q", got, "0123456789")
	}

	// Further writes after truncation are no-ops.
	c.Write([]byte("more data that must not appear"))
	if got := string(c.Bytes()); got != "0123456789" {
		t.Errorf("Bytes() after truncation = %q, want unchanged %q", got, "0123456789")
	}
}

func TestCapBuffer_PartialWriteAtBoundary(t *testing.T) {
	c := newCapBuffer(5)
	c.Write([]byte("abcdefgh")) // 8 bytes into a 5 byte cap

	if !c.truncated {
		t.Fatal("truncated = false, want true")
	}
	if got := string(c.Bytes()); got != "abcde" {
		t.Errorf("Bytes() = %q, want %q (first 5 bytes kept)", got, "abcde")
	}
}
