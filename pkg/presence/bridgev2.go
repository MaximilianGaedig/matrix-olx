package presence

import (
	"context"
	"fmt"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
)

// GhostSender returns a SendFunc that sets presence on existing bridgev2
// ghosts. bridgev2 has no presence API, so this reaches through the Matrix
// connector's appservice intent. Ghosts that don't exist yet are skipped
// rather than created, so presence alone never materializes new ghosts.
func GhostSender(br *bridgev2.Bridge) SendFunc {
	return func(ctx context.Context, remoteUserID string, p event.Presence) error {
		ghost, err := br.GetExistingGhostByID(ctx, networkid.UserID(remoteUserID))
		if err != nil {
			return fmt.Errorf("failed to get ghost: %w", err)
		} else if ghost == nil || ghost.Intent == nil {
			return nil
		}
		return SetGhostPresence(ctx, ghost, p)
	}
}

// SetGhostPresence sets the Matrix presence of a single ghost: the presence
// alone, the request has no status_msg.
func SetGhostPresence(ctx context.Context, ghost *bridgev2.Ghost, p event.Presence) error {
	as, ok := ghost.Intent.(*matrix.ASIntent)
	if !ok || as.Matrix == nil {
		return fmt.Errorf("ghost intent is %T, not an appservice intent", ghost.Intent)
	}
	if err := as.Matrix.EnsureRegistered(ctx); err != nil {
		return fmt.Errorf("failed to ensure ghost is registered: %w", err)
	}
	return as.Matrix.SetPresence(ctx, mautrix.ReqPresence{Presence: p})
}
