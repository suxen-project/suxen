package content

import (
	"math"
	"testing"
	"time"

	"github.com/suxen-project/suxen/internal/config"
)

func TestWebhookRetryDelaySaturatesWithoutOverflow(t *testing.T) {
	for _, test := range []struct {
		name    string
		base    time.Duration
		attempt int
		want    time.Duration
	}{
		{"default first", 0, 1, time.Second},
		{"default eighth", 0, 8, 128 * time.Second},
		{"subsecond long retry", time.Millisecond, 20, 524288 * time.Millisecond},
		{"subsecond cap", time.Millisecond, 23, time.Hour},
		{"huge base", time.Duration(math.MaxInt64), 2, time.Hour},
		{"huge attempt", time.Nanosecond, math.MaxInt, time.Hour},
		{"below cap", time.Hour / 2, 2, time.Hour},
		{"negative attempt", time.Second, -1, time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			rt := &Runtime{Config: config.Config{WebhookRetryBase: test.base}}
			if got := rt.webhookRetryDelay(test.attempt); got != test.want {
				t.Fatalf("delay = %v, want %v", got, test.want)
			}
		})
	}
}
