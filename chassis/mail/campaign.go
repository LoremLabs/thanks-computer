package mail

import (
	"context"

	"go.uber.org/zap"
)

// claimCampaign attempts to claim (tenant, campaign, recipient) in
// mail_campaign_sends. It returns claimed=true only when this call created
// the row (INSERT actually inserted) — the at-most-once guard. claimed=false
// means the recipient was already claimed/sent for this campaign → the
// caller skips the send. Writes go to the REAL runtime *sql.DB (not the
// dbcache snapshot) so the just-written row is visible to a subsequent claim
// in the same request.
func (m *Mailer) claimCampaign(ctx context.Context, tenant, campaign, recipient, now string) (claimed bool, err error) {
	res, err := m.db.ExecContext(ctx,
		`INSERT INTO mail_campaign_sends (tenant_id, campaign, recipient, status, sent_at)
		 VALUES (?, ?, ?, 'claimed', ?)
		 ON CONFLICT (tenant_id, campaign, recipient) DO NOTHING`,
		tenant, campaign, recipient, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// markCampaignSent promotes a claimed row to sent with its message id.
func (m *Mailer) markCampaignSent(ctx context.Context, tenant, campaign, recipient, messageID, now string) error {
	_, err := m.db.ExecContext(ctx,
		`UPDATE mail_campaign_sends
		    SET status = 'sent', message_id = ?, sent_at = ?
		  WHERE tenant_id = ? AND campaign = ? AND recipient = ?`,
		messageID, now, tenant, campaign, recipient)
	return err
}

// releaseCampaign deletes a claim whose send failed, so a retry can re-send.
// Best-effort: a failure here only risks one recipient being stuck-claimed
// (the at-most-once bias), never a double send.
func (m *Mailer) releaseCampaign(ctx context.Context, tenant, campaign, recipient string) {
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM mail_campaign_sends
		  WHERE tenant_id = ? AND campaign = ? AND recipient = ? AND status = 'claimed'`,
		tenant, campaign, recipient); err != nil && m.log != nil {
		m.log.Warn("sendmail: release campaign claim failed", zap.Error(err))
	}
}
