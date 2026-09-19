package nbuspub

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

const receiptWorkerMode = "receipt-fd-v1"

func prepareEnvelope(channel, source, payload string) ([]byte, error) {
	if channel == "" || !json.Valid([]byte(payload)) {
		return nil, fmt.Errorf("invalid channel/payload")
	}
	return json.Marshal(envelope{
		SpecVersion: "1.0", ID: semanticEventID(payload), Source: source, Type: channel,
		Time:            time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		DataContentType: "application/json", Data: json.RawMessage(payload),
	})
}

func syncAndDeliverReceipt(receipt *os.File, raw []byte) error {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("invalid receipt: %w", err)
	}
	if env.SpecVersion != "1.0" || env.ID == "" || env.Type == "" || !json.Valid(env.Data) {
		return fmt.Errorf("incomplete receipt envelope")
	}
	st, err := receipt.Stat()
	if err != nil {
		return fmt.Errorf("receipt descriptor: %w", err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("receipt is not a regular file")
	}
	if err := debugLogSync(receipt); err != nil {
		return fmt.Errorf("receipt sync: %w", err)
	}
	if os.Getenv("NERVOUS_NO_REDIS") == "1" {
		return nil
	}
	// Keep the persisted ID, time and bytes. Rebuilding the envelope previously
	// gave payloads without an event_id a second random ID, defeating dedup.
	if err := redisDeliver(env, raw); err != nil {
		return fmt.Errorf("delivery deferred to receipt mirror: %w", err)
	}
	return nil
}
