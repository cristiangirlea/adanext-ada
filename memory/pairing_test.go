package memory

import (
	"testing"
	"time"
)

func TestUnpairRevokesCodeAndKeepsCooldown(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := Pairing{Channel: "echo", Chat: "room", Sender: "reader"}
	code, err := s.OfferPairing(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Unlink(p.Channel, p.Sender); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pair(code, "owner"); err == nil {
		t.Fatal("unpair left an active pairing code")
	}
	if code, err := s.OfferPairing(p); err != nil || code != "" {
		t.Fatalf("pairing cooldown lost: %q %v", code, err)
	}
}
func TestExpiredPairing(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := Pairing{Channel: "echo", Chat: "room", Sender: "reader", Name: "Jo"}
	before := time.Now().Add(-11 * time.Minute)
	code, err := s.offerPairing(p, before)
	if err != nil || len(code) != 6 {
		t.Fatalf("offer: %q %v", code, err)
	}
	if _, err = s.Pair(code, "owner"); err == nil {
		t.Fatal("expired code accepted")
	}
	if _, ok := s.Resolve("echo", "reader"); ok {
		t.Fatal("expired code linked")
	}
	again, err := s.OfferPairing(p)
	if err != nil || again == "" {
		t.Fatalf("renew: %q %v", again, err)
	}
	if got, err := s.OfferPairing(p); err != nil || got != "" {
		t.Fatalf("repeated offer: %q %v", got, err)
	}
}
