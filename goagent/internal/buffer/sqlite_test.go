package buffer

import (
	"context"
	"testing"
)

func TestBufferDrainAndApplied(t *testing.T) {
	ctx := context.Background()
	b, err := Open(":memory:", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Enqueue(ctx, "telemetry", []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	var sent int
	if err := b.Drain(ctx, func(kind string, body []byte) error {
		sent++
		if kind != "telemetry" {
			t.Fatalf("kind = %s", kind)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatalf("sent = %d", sent)
	}
	n, _ := b.Count(ctx)
	if n != 0 {
		t.Fatalf("outbox count = %d", n)
	}
	if err := b.MarkApplied(ctx, "cmd-1", "applied", []byte(`{"task_id":"t1"}`)); err != nil {
		t.Fatal(err)
	}
	ok, status, _, err := b.AlreadyApplied(ctx, "cmd-1")
	if err != nil || !ok || status != "applied" {
		t.Fatalf("applied lookup ok=%v status=%s err=%v", ok, status, err)
	}
}
