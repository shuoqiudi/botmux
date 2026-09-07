package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/skrashevich/botmux/internal/models"
)

type dlqOrderStore struct {
	delivery models.InboundDelivery
	events   []string
}

func (s *dlqOrderStore) PrepareInboundDelivery(context.Context, int64, int64, string, int64, []byte) (*models.InboundDelivery, bool, error) {
	return nil, false, errors.New("unused")
}
func (s *dlqOrderStore) GetInboundDelivery(context.Context, string) (*models.InboundDelivery, error) {
	copy := s.delivery
	return &copy, nil
}
func (s *dlqOrderStore) SetInboundStreamID(context.Context, string, string) error { return nil }
func (s *dlqOrderStore) BeginInboundAttempt(context.Context, string, string) (int, error) {
	s.delivery.AttemptCount++
	return s.delivery.AttemptCount, nil
}
func (s *dlqOrderStore) CompleteInboundAttempt(context.Context, string, string, int) error {
	return errors.New("unused")
}
func (s *dlqOrderStore) FailInboundAttempt(_ context.Context, _, _, class string, _ int, _ time.Time) error {
	s.delivery.Status = models.InboundRetrying
	s.delivery.LastErrorClass = class
	return nil
}
func (s *dlqOrderStore) MarkInboundDLQ(_ context.Context, _, class string) error {
	s.events = append(s.events, "sqlite-dlq")
	s.delivery.Status = models.InboundDLQ
	s.delivery.LastErrorClass = class
	return nil
}

type dlqOrderQueue struct {
	store     *dlqOrderStore
	moveCalls int
}

func (*dlqOrderQueue) AppendUnique(context.Context, string) (string, error) {
	return "", errors.New("unused")
}
func (*dlqOrderQueue) EnsureGroup(context.Context) error { return nil }
func (*dlqOrderQueue) ReadNew(context.Context, string, time.Duration, int64) ([]StreamMessage, error) {
	return nil, nil
}
func (*dlqOrderQueue) ClaimStale(context.Context, string, time.Duration, int64) ([]StreamMessage, error) {
	return nil, nil
}
func (*dlqOrderQueue) Ack(context.Context, string) error {
	return errors.New("source must not be acked directly")
}
func (q *dlqOrderQueue) MoveToDLQAndAck(context.Context, StreamMessage, string, int) error {
	q.moveCalls++
	if len(q.store.events) == 0 || q.store.events[0] != "sqlite-dlq" {
		return errors.New("source ack attempted before durable operator record")
	}
	if q.moveCalls == 1 {
		return errors.New("simulated crash before Redis DLQ move and XACK")
	}
	q.store.events = append(q.store.events, "redis-dlq-and-xack")
	return nil
}

func TestInboundDLQRecordPrecedesXACKAndReclaimCompletesMove(t *testing.T) {
	store := &dlqOrderStore{delivery: models.InboundDelivery{
		DeliveryID: "delivery", Status: models.InboundRetrying, AttemptCount: 1,
	}}
	queue := &dlqOrderQueue{store: store}
	inbound := NewInbound(store, queue, nil, InboundConfig{MaxAttempts: 2})
	message := StreamMessage{ID: "1-0", DeliveryID: "delivery"}
	if err := inbound.process(context.Background(), message); err == nil {
		t.Fatal("simulated crash window did not interrupt the first DLQ move")
	}
	if store.delivery.Status != models.InboundDLQ || len(store.events) != 1 {
		t.Fatalf("operator record was not durable before XACK: delivery=%+v events=%v", store.delivery, store.events)
	}
	if err := inbound.process(context.Background(), message); err != nil {
		t.Fatalf("reclaim did not finish idempotent DLQ move: %v", err)
	}
	if queue.moveCalls != 2 || len(store.events) != 2 || store.events[1] != "redis-dlq-and-xack" {
		t.Fatalf("DLQ recovery order=%v move_calls=%d", store.events, queue.moveCalls)
	}
}
