package authrecovery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
)

// Called under the same operation mutex as all challenge/bank submissions.
// Durable fence is already committed by the Telegram callback; browser cleanup
// must wait until revoke has had its one opportunity to use the active page.
func (c *Coordinator) reconcileLogout(ctx context.Context) (bool, error) {
	j, err := c.Store.OpenACBLogoutJob(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer clear(j.SessionEnvelope)
	if err := c.Store.CheckMutationAllowed(ctx); err != nil {
		return true, err
	}
	if err := c.Store.CheckACBLogoutFence(ctx, j.ConnectionID, j.FencedGeneration); err != nil {
		return true, err
	}
	if err := c.Store.RecoverACBLogoutRevocation(ctx, j.ID, c.Now()); err != nil {
		return true, err
	}
	clear(j.SessionEnvelope)
	j, err = c.Store.ACBLogoutJob(ctx, j.ID)
	if err != nil {
		return true, err
	}
	defer clear(j.SessionEnvelope)
	if j.LocalClearedAt == "" && c.InvalidateSession != nil {
		invalidateCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = c.InvalidateSession(invalidateCtx, j.ConnectionID, j.FencedGeneration)
		cancel()
		if err == nil {
			if err := c.Store.MarkACBLogoutLocalCleared(ctx, j.ID); err != nil {
				return true, err
			}
		}
	}
	if j.BankStatus == "PENDING" {
		status, reason := "UNCONFIRMED", "NO_SESSION_SNAPSHOT"
		handoff := ""
		if len(j.SessionEnvelope) > 0 {
			reason = "SESSION_DECRYPT_FAILED"
			if c.Keyring != nil {
				var envelope security.Envelope
				if json.Unmarshal(j.SessionEnvelope, &envelope) == nil && envelope.KeyID == j.SessionKeyID {
					plaintext, decryptErr := c.Keyring.Decrypt(envelope, security.SessionAAD(j.ConnectionID, j.SessionGeneration))
					if errors.Is(decryptErr, security.ErrAuthenticationFailed) {
						plaintext, decryptErr = c.Keyring.Decrypt(envelope, security.LegacySessionAAD(j.ConnectionID))
					}
					if decryptErr == nil {
						handoff = string(plaintext)
					}
					clear(plaintext)
				}
			}
		}
		// An active authenticated page can revoke without a persisted handoff. A
		// corrupt snapshot must never be sent, but does not destroy that opportunity.
		if handoff != "" || j.AttemptID != "" {
			if err := c.Store.BeginACBLogoutRevocation(ctx, j.ID); err != nil {
				return true, err
			}
			revokeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			result, revokeErr := c.Browser.RevokeSession(revokeCtx, j.ID, j.AttemptID, handoff)
			handoff = ""
			cancel()
			reason = "LOGOUT_OUTCOME_UNKNOWN"
			if revokeErr == nil {
				switch result.Status {
				case "CONFIRMED", "ALREADY_EXPIRED", "UNCONFIRMED":
					status = result.Status
					reason = safeRevocationReason(result.ReasonCode)
				}
			}
		}
		if err := c.Store.FinishACBLogoutRevocation(ctx, j.ID, status, reason); err != nil {
			return true, err
		}
	}
	// At this point bank outcome is durable (possibly unknown after a crash).
	// Local-only retries must never repeat the bank click.
	if j.AttemptID != "" {
		cleanupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = c.Browser.Cancel(cleanupCtx, j.AttemptID)
		cancel()
		delete(c.windows, j.AttemptID)
		delete(c.verifyAfter, j.AttemptID)
		delete(c.submissions, j.AttemptID)
	}
	clear(c.liveChallenges)
	return true, nil
}

func safeRevocationReason(reason string) string {
	switch reason {
	case "", "LOGOUT_SNAPSHOT_INVALID", "LOGOUT_NO_SESSION", "LOGOUT_BROWSER_UNAVAILABLE", "LOGOUT_CONTROL_UNSUPPORTED", "LOGOUT_OUTCOME_UNKNOWN", "LOGOUT_PROBE_UNSUPPORTED", "LOGOUT_PROBE_FAILED", "LOGOUT_NOT_CONFIRMED", "SESSION_ALREADY_EXPIRED", "SESSION_REVOKED":
		return reason
	default:
		return "LOGOUT_NOT_CONFIRMED"
	}
}
