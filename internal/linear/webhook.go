package linear

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"time"
)

const (
	maxBodySize       = 1 << 20 // 1 MB
	signatureHeader   = "Linear-Signature"
	timestampHeader   = "Linear-Delivery"
	maxTimestampDrift = 60 * time.Second
)

// DispatchFunc is the callback the webhook handler invokes for valid payloads.
type DispatchFunc func(payload WebhookPayload)

// NewWebhookHandler returns an http.HandlerFunc that verifies and dispatches Linear webhooks.
func NewWebhookHandler(secret string, dispatch DispatchFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
		if err != nil {
			slog.Error("reading webhook body", "error", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Verify HMAC-SHA256 signature
		sig := r.Header.Get(signatureHeader)
		if sig == "" {
			slog.Warn("missing webhook signature")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !verifySignature(secret, body, sig) {
			slog.Warn("invalid webhook signature")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// Validate timestamp freshness
		if ts := r.Header.Get(timestampHeader); ts != "" {
			deliveryTime, err := time.Parse(time.RFC3339Nano, ts)
			if err == nil {
				drift := time.Since(deliveryTime)
				if math.Abs(drift.Seconds()) > maxTimestampDrift.Seconds() {
					slog.Warn("webhook timestamp too old", "drift", drift)
					http.Error(w, "request too old", http.StatusBadRequest)
					return
				}
			}
		}

		var payload WebhookPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			slog.Error("parsing webhook payload", "error", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Return 200 immediately
		w.WriteHeader(http.StatusOK)

		// Filter: only Issue updates and Comment creates
		switch {
		case payload.Type == "Issue" && payload.Action == "update":
			go dispatch(payload)
		case payload.Type == "Comment" && payload.Action == "create":
			go dispatch(payload)
		default:
			slog.Debug("ignoring webhook", "type", payload.Type, "action", payload.Action)
		}
	}
}

func verifySignature(secret string, body []byte, signature string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}
