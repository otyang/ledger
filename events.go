package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// Event types. Data holds the object after the change, as returned by the
// API: a Transaction, Hold, Account, Ledger, Batch, Currency or PeriodClose.
const (
	EventCurrencyRegistered = "currency.registered"
	EventLedgerCreated      = "ledger.created"
	EventAccountCreated     = "account.created"
	EventAccountUpdated     = "account.updated"
	EventTransactionPosted  = "transaction.posted"
	EventTransactionUpdated = "transaction.updated"
	EventHoldCreated        = "hold.created"
	EventHoldCommitted      = "hold.committed"
	EventHoldVoided         = "hold.voided"
	EventHoldExpired        = "hold.expired"
	EventBatchPosted        = "batch.posted"
	EventPeriodClosed       = "period.closed"
)

// Event records one change. Events are written in the same database
// transaction as the change, so they exist exactly when the change does.
type Event struct {
	bun.BaseModel `bun:"table:ledger.events,alias:ev"`

	Seq       int64           `bun:",pk,autoincrement" json:"-"`
	ID        EventID         `bun:",notnull" json:"id"`
	Type      string          `bun:",notnull" json:"type"`
	LedgerID  *LedgerID       `bun:",nullzero" json:"ledger_id,omitempty"`
	SubjectID string          `bun:",notnull" json:"subject_id"`
	Data      json.RawMessage `bun:",type:jsonb,notnull" json:"data"`
	CreatedAt time.Time       `bun:",notnull" json:"created_at"`
}

// emit records an event in tx.
func emit(ctx context.Context, tx bun.IDB, typ string, ledgerID *LedgerID, subjectID string, data any, at time.Time) error {
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("ledger: encode %s event: %w", typ, err)
	}
	ev := &Event{ID: NewEventID(), Type: typ, LedgerID: ledgerID, SubjectID: subjectID, Data: b, CreatedAt: at}
	if _, err := tx.NewInsert().Model(ev).ExcludeColumn("seq").Exec(ctx); err != nil {
		return fmt.Errorf("ledger: record %s event: %w", typ, err)
	}
	return nil
}

type ListEventsParams struct {
	// After is the pagination cursor: the ID of the last event received.
	// Empty starts from the oldest event.
	After EventID
	// LedgerID, if set, returns only that ledger's events.
	LedgerID LedgerID
	// Limit defaults to 100 and is capped at 1000.
	Limit int
}

// ListEvents returns events in a stable order that is safe to tail: an event
// never appears behind a cursor that was already handed out. Like entries of
// untracked accounts, events are ordered by the database transaction that
// wrote them, and only shown once every older transaction has finished, so a
// long-running database transaction delays new events.
func (s *Service) ListEvents(ctx context.Context, p ListEventsParams) ([]*Event, error) {
	events, err := s.listEvents(ctx, p)
	return events, classify(ctx, err)
}

func (s *Service) listEvents(ctx context.Context, p ListEventsParams) ([]*Event, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	limit = min(limit, maxListLimit)

	events := make([]*Event, 0)
	q := s.db.NewSelect().Model(&events).
		ColumnExpr("ev.seq, ev.id, ev.type, ev.ledger_id, ev.subject_id, ev.data, ev.created_at").
		Where("ev.txid < pg_snapshot_xmin(pg_current_snapshot())::text::bigint").
		OrderExpr("ev.txid, ev.seq").
		Limit(int64(limit))
	if p.After != "" {
		if err := parseID(prefixEvent, string(p.After)); err != nil {
			return nil, err
		}
		var txid, seq int64
		err := s.db.NewSelect().TableExpr("ledger.events").ColumnExpr("txid, seq").
			Where("id = ?", p.After).Scan(ctx, &txid, &seq)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: cursor %s is not an event", ErrInvalidParams, p.After)
		}
		if err != nil {
			return nil, fmt.Errorf("ledger: list events: %w", err)
		}
		q = q.Where("(ev.txid, ev.seq) > (?, ?)", txid, seq)
	}
	if p.LedgerID != "" {
		if err := parseID(prefixLedger, string(p.LedgerID)); err != nil {
			return nil, err
		}
		q = q.Where("ev.ledger_id = ?", p.LedgerID)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("ledger: list events: %w", err)
	}
	return events, nil
}

// EventCursor returns the last event a named consumer (e.g. a webhook
// dispatcher) has processed, or "" if it hasn't processed any.
func (s *Service) EventCursor(ctx context.Context, name string) (EventID, error) {
	return s.EventCursorTx(ctx, s.db, name)
}

// EventCursorTx is EventCursor through db, e.g. a pinned connection.
func (s *Service) EventCursorTx(ctx context.Context, db bun.IDB, name string) (EventID, error) {
	var id EventID
	err := db.NewSelect().TableExpr("ledger.webhook_cursors").Column("event_id").Where("name = ?", name).Scan(ctx, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", classify(ctx, fmt.Errorf("ledger: read event cursor: %w", err))
	}
	return id, nil
}

// SetEventCursor records that a named consumer has processed every event up
// to and including id. A cursor only moves forward: setting it to an event
// before the current one does nothing.
func (s *Service) SetEventCursor(ctx context.Context, name string, id EventID) error {
	return s.SetEventCursorTx(ctx, s.db, name, id)
}

// SetEventCursorTx is SetEventCursor through db, e.g. a pinned connection.
func (s *Service) SetEventCursorTx(ctx context.Context, db bun.IDB, name string, id EventID) error {
	if err := parseID(prefixEvent, string(id)); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO ledger.webhook_cursors AS c (name, event_id, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET event_id = EXCLUDED.event_id, updated_at = EXCLUDED.updated_at
		WHERE EXISTS (SELECT 1 FROM ledger.events n, ledger.events o
		               WHERE n.id = EXCLUDED.event_id AND o.id = c.event_id
		                 AND (n.txid, n.seq) > (o.txid, o.seq))`,
		name, id, now())
	if err != nil {
		return classify(ctx, fmt.Errorf("ledger: save event cursor: %w", err))
	}
	return nil
}
