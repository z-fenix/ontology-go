package action

import (
	"context"
	"log/slog"
)

// LogNotifier records notifications in the service log. Replace with an email/chat integration.
type LogNotifier struct{}

// Notify implements workflow.Notifier.
func (LogNotifier) Notify(ctx context.Context, to, template string, data map[string]any) error {
	slog.InfoContext(ctx, "notification", "to", to, "template", template, "data", data)
	return nil
}
