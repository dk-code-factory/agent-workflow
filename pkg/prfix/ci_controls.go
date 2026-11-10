package prfix

import (
	"fmt"
	"math"
	"time"
)

// The source schema permits arbitrary integers; this runtime must reject values
// it cannot represent before claiming work, rather than overflow their windows.
func validateCIOptions(options map[string]any) error {
	for key, minimum := range map[string]float64{"timeout_seconds": 0, "poll_interval_seconds": 1, "classification_timeout_seconds": 1, "no_checks_grace_seconds": 0, "pending_extension_seconds": 0} {
		value, exists := options[key]
		if !exists {
			continue
		}
		n, ok := value.(float64)
		if !ok || math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n || n < minimum || n > float64(math.MaxInt64/int64(time.Second)) {
			return fmt.Errorf("CI %s exceeds supported integral duration range", key)
		}
	}
	return nil
}
func ciSeconds(options map[string]any, key string, fallback time.Duration) time.Duration {
	if n, ok := options[key].(float64); ok {
		return time.Duration(int64(n)) * time.Second
	}
	return fallback
}
