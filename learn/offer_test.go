package learn

import (
	"testing"
	"time"

	"github.com/cristiangirlea/adanext-ada/memory"
	"github.com/cristiangirlea/adanext-ada/provider"
)

func setupPrivateOffer(t *testing.T) fixture {
	t.Helper()
	f := setup(t)
	origin := memory.Origin{Channel: "cli", Chat: "local", Sender: "me", Private: true}
	msg := provider.Message{Role: "user", Content: []provider.Block{{Type: "text", Text: "I like tea and coffee"}}}
	if err := f.Memory.AppendAt(f.session, origin, "", msg); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestDeliveryCannotBindTwoProposals(t *testing.T) {
	f := setupPrivateOffer(t)
	model := &fake{reply: `{"facts":[{"text":"tea","messages":[0]},{"text":"coffee","messages":[0]}]}`}
	items, err := Propose(t.Context(), model, f.Store, "owner", f.session)
	if err != nil || len(items) != 2 {
		t.Fatal(items, err)
	}
	offer := Offer{Channel: "cli", Chat: "local", Sender: "me", DeliveryID: "same", Expires: time.Now().UTC().Add(time.Hour)}
	if ok, err := f.BindOffer("owner", items[0].ID, offer); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := f.BindOffer("owner", items[1].ID, offer); err == nil || ok {
		t.Fatal("reused delivery", ok, err)
	}
	pending, err := f.List("owner", false)
	if err != nil || len(pending) != 2 || pending[1].Offer != nil {
		t.Fatal(pending, err)
	}
}

func TestOfferDecisionRecovery(t *testing.T) {
	f := setupPrivateOffer(t)
	items, err := Propose(t.Context(), &fake{reply: one("likes tea", "[0]")}, f.Store, "owner", f.session)
	if err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
	offer := Offer{Channel: "cli", Chat: "local", Sender: "me", DeliveryID: "delivery", Expires: time.Now().UTC().Add(time.Hour)}
	ok, err := f.BindOffer("owner", items[0].ID, offer)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	press := ButtonPress{Origin: memory.Origin{Channel: "cli", Chat: "local", Sender: "me"}, DeliveryID: "delivery", Button: "keep", InboundID: "press"}
	for _, tc := range []struct {
		replay  bool
		id      string
		confirm bool
	}{
		{false, "press", true}, {false, "new-press", false}, {true, "new-press", false}, {true, "press", true},
	} {
		press.Replay, press.InboundID = tc.replay, tc.id
		ok, err := f.DecideOffer("owner", press)
		if err != nil || ok != tc.confirm {
			t.Fatal(tc, ok, err)
		}
	}
	if facts := f.Memory.Facts("owner"); len(facts) != 1 {
		t.Fatal(facts)
	}
}
