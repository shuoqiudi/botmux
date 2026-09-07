package gateway

import "time"

// retryBackoff returns bounded exponential backoff with stable +/-20% jitter.
// Stable jitter prevents synchronized retries while keeping fault tests
// deterministic for a given Delivery ID.
func retryBackoff(deliveryID string, attempt int, base, maximum time.Duration) time.Duration {
	delay := base
	for n := 1; n < attempt && delay < maximum; n++ {
		delay *= 2
	}
	if delay > maximum {
		delay = maximum
	}
	var hash uint32
	for _, char := range deliveryID {
		hash = hash*33 + uint32(char)
	}
	jitter := (float64(int(hash%41)-20) / 100.0) + 1
	return time.Duration(float64(delay) * jitter)
}
