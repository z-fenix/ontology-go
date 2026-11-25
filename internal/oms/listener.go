package oms

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// VersionSink receives (ontology, version) on each oms_version notification.
type VersionSink interface {
	OnVersionChange(version int64)
}

// ListenVersions LISTENs on the oms_version channel and forwards notifications to sink.
// It reconnects on error with a short backoff. Spec §5.1 / §5.6.
func ListenVersions(ctx context.Context, connect func(context.Context) (*pgx.Conn, error), ontology string, sink VersionSink) {
	for ctx.Err() == nil {
		if err := listenOnce(ctx, connect, ontology, sink); err != nil {
			slog.WarnContext(ctx, "oms listener error, reconnecting", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func listenOnce(ctx context.Context, connect func(context.Context) (*pgx.Conn, error), ontology string, sink VersionSink) error {
	conn, err := connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "LISTEN oms_version"); err != nil {
		return err
	}
	for ctx.Err() == nil {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		name, verStr, ok := strings.Cut(n.Payload, ":")
		if !ok || (ontology != "" && name != ontology) {
			continue
		}
		if v, err := strconv.ParseInt(verStr, 10, 64); err == nil {
			sink.OnVersionChange(v)
		}
	}
	return ctx.Err()
}
