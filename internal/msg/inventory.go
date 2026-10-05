package msg

import (
	"context"

	"github.com/muthuishere/herdr-expose/internal/inventory"
)

// InventorySource adapts this package's Herdr transport to what the inventory
// needs, so internal/inventory depends on nothing but its own types.
//
// Agents comes from the same attach-free listing messaging already uses, and
// Screen is a pane.read, which takes no geometry either. An inventory is the
// most read-heavy thing in the product and it must still not touch a pane.
type InventorySource struct {
	Herdr  Herdr
	Reader func(ctx context.Context, session, pane string, lines int) (string, error)
}

func (s InventorySource) Agents(ctx context.Context) ([]inventory.Agent, error) {
	list, err := s.Herdr.Agents(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]inventory.Agent, 0, len(list))
	for _, a := range list {
		addr := a.Address
		if addr == "" {
			addr = a.Session + "/" + a.PaneID
		}
		out = append(out, inventory.Agent{
			Address: addr, Session: a.Session, Name: a.Name, PaneID: a.PaneID,
			Kind: a.Kind, Cwd: a.Cwd, Title: a.Title, Status: a.Status,
		})
	}
	return out, nil
}

func (s InventorySource) Screen(ctx context.Context, session, pane string, lines int) (string, error) {
	if s.Reader == nil {
		return "", nil
	}
	return s.Reader(ctx, session, pane, lines)
}
