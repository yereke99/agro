// handler/payment-handler.go
package handler

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// CheckPayment запускает фоновой цикл, который раз в сутки
// проверяет просроченные подписки и помечает их как expired.
func (h *Handler) CheckPayment(ctx context.Context) {
	h.logger.Info("started check payment handler")

	// Сразу одна проверка при старте
	h.checkAndExpireSubscriptions(ctx)

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			h.logger.Info("stopping check payment handler", zap.Error(ctx.Err()))
			return
		case <-ticker.C:
			h.logger.Info("checking payment date for each user")
			h.checkAndExpireSubscriptions(ctx)
		}
	}
}

// checkAndExpireSubscriptions находит все подписки, у которых valid_until < NOW(),
// и помечает:
//   - subscriptions.status = 'expired'
//   - users.sub_status = 'expired', users.sub_until = NULL
func (h *Handler) checkAndExpireSubscriptions(ctx context.Context) {
	if h.db == nil {
		h.logger.Warn("db is nil in checkAndExpireSubscriptions")
		return
	}

	now := time.Now()

	// 1) Помечаем просроченные записи в subscriptions
	resSub, err := h.db.ExecContext(ctx, `
		UPDATE subscriptions
		SET status = 'expired'
		WHERE status = 'active'
		  AND valid_until IS NOT NULL
		  AND valid_until < ?
	`, now)
	if err != nil {
		h.logger.Error("expire subscriptions", zap.Error(err))
	} else {
		if n, _ := resSub.RowsAffected(); n > 0 {
			h.logger.Info("expired subscriptions updated", zap.Int64("count", n))
		}
	}

	// 2) Помечаем просроченные подписки в users
	resUsers, err := h.db.ExecContext(ctx, `
		UPDATE users
		SET sub_status = 'expired',
		    sub_until  = NULL,
		    updated_at = CURRENT_TIMESTAMP
		WHERE sub_status = 'active'
		  AND sub_until IS NOT NULL
		  AND sub_until < ?
	`, now)
	if err != nil {
		h.logger.Error("expire user subscriptions", zap.Error(err))
	} else {
		if n, _ := resUsers.RowsAffected(); n > 0 {
			h.logger.Info("users sub_status expired", zap.Int64("count", n))
		}
	}
}
