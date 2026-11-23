package learn

import (
	"fmt"
	"slices"
	"time"

	"github.com/cristiangirlea/adanext-ada/memory"
)

type Offer struct {
	Channel    string    `json:"channel"`
	Chat       string    `json:"chat"`
	Sender     string    `json:"sender"`
	DeliveryID string    `json:"delivery_id"`
	Expires    time.Time `json:"expires"`
}

func (o Offer) valid() bool {
	return memory.ValidIdentity(o.Channel) && memory.ValidIdentity(o.Sender) && memory.ValidIdentity(o.Chat) &&
		memory.ValidIdentity(o.DeliveryID) && !o.Expires.IsZero()
}

func (o Offer) matches(origin memory.Origin) bool {
	return o.Channel == origin.Channel && o.Chat == origin.Chat && o.Sender == origin.Sender
}

// BindOffer commits the button binding before the plugin can see the delivery.
func (s Store) BindOffer(user, id string, offer Offer) (bool, error) {
	offer.Expires = offer.Expires.UTC()
	if !offer.valid() {
		return false, fmt.Errorf("invalid learning offer")
	}
	var send bool
	err := s.withUser(user, func(tx *transaction) error {
		p, err := tx.find(id)
		if err != nil {
			return err
		}
		if p.Status != "pending" || !time.Now().Before(offer.Expires) {
			return nil
		}
		if current, ok := s.Memory.Resolve(offer.Channel, offer.Sender); !ok || current != user {
			return nil
		}
		// A recovered outbox may predate private-chat evidence on session origins.
		session, err := s.Memory.Get(p.Session)
		if err != nil {
			return err
		}
		if session.User != user || !slices.ContainsFunc(session.Origins, func(origin memory.Origin) bool {
			return origin.Private && offer.matches(origin)
		}) {
			return nil
		}
		if p.Offer != nil {
			send = *p.Offer == offer
			return nil
		}
		for _, other := range tx.Proposals {
			if other.Offer != nil && other.Offer.DeliveryID == offer.DeliveryID {
				return fmt.Errorf("duplicate offer delivery ID")
			}
		}
		p.Offer = &offer
		send = true
		return tx.save()
	})
	return send && err == nil, err
}

type ButtonPress struct {
	memory.Origin
	DeliveryID, Button, InboundID string
	Replay                        bool
}

func (s Store) DecideOffer(user string, press ButtonPress) (bool, error) {
	if !memory.ValidIdentity(press.InboundID) || (press.Button != "keep" && press.Button != "reject") {
		return false, nil
	}
	var decided bool
	err := s.withUser(user, func(tx *transaction) error {
		for i := range tx.Proposals {
			p := &tx.Proposals[i]
			if p.Offer == nil || p.Offer.DeliveryID != press.DeliveryID || !p.Offer.matches(press.Origin) {
				continue
			}
			if current, ok := s.Memory.Resolve(press.Channel, press.Sender); !ok || current != user {
				return nil
			}
			if p.Status != "pending" {
				// Only an unfinished inbox entry can still be missing its confirmation delivery.
				if press.Replay && p.DecisionInbound == press.InboundID {
					decided = press.Button == "keep" && p.Status != "rejected" || press.Button == "reject" && p.Status == "rejected"
				}
				return nil
			}
			now := time.Now()
			if !now.Before(p.Offer.Expires) {
				return nil
			}
			p.DecisionInbound = press.InboundID
			// The outside offer's Keep anyway button explicitly authorizes this same profile transition.
			if err := tx.change(p, press.Button, now); err != nil {
				return err
			}
			decided = true
			break
		}
		return nil
	})
	return decided, err
}
