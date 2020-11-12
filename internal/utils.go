package resttunnel

import (
	"math"
	"math/rand"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/valyala/fasthttp"
)

// IsDiscordAPIURI returns a boolean if the current URI is a discord API endpoint
func IsDiscordAPIURI(URI *fasthttp.URI) bool {
	_, ok := discordDomains[string(URI.Host())]
	return ok && string(URI.Path()[:4]) == "/api"
}

// DurationTimestamp outputs in a format similar to the timestamp String()
func DurationTimestamp(d time.Duration) (output string) {
	seconds := d.Seconds()
	if seconds > 86400 {
		days := math.Trunc(seconds / 86400)
		if days > 0 {
			output += strconv.Itoa(int(days)) + "d"
		}
		seconds = math.Mod(seconds, 86400)
	}
	if seconds > 3600 {
		hours := math.Trunc(seconds / 3600)
		if hours > 0 {
			output += strconv.Itoa(int(hours)) + "h"
		}
		seconds = math.Mod(seconds, 3600)
	}
	minutes := math.Trunc(seconds / 60)
	if minutes > 0 {
		output += strconv.Itoa(int(minutes)) + "m"
	}
	seconds = math.Mod(seconds, 60)
	if seconds > 0 {
		output += strconv.Itoa(int(seconds)) + "s"
	}
	return
}

// createLineChart creates a LineChart from an accumulator
func createLineChart(ac *Accumulator, background string, border string) (chart LineChart) {
	data := make([]interface{}, 0, len(ac.Samples))
	for _, sample := range ac.Samples {
		data = append(data, DataStamp{sample.StoredAt, sample.Value})
	}
	chart = LineChart{
		Datasets: []Dataset{{
			Label:            ac.Label,
			BackgroundColour: background,
			BorderColour:     border,
			Data:             data,
		}},
	}
	return chart
}

// randomCallback returns a random callback from callbacks
func (rt *RestTunnel) randomCallback() (k uuid.UUID, v *TunnelResponse) {
	rt.callbacksMu.RLock()
	defer rt.callbacksMu.RUnlock()

	if len(rt.Callbacks) < 1 {
		return
	}

	i := rand.Intn(len(rt.Callbacks))
	for k, v = range rt.Callbacks {
		if i == 0 {
			return k, v
		}
		i--
	}
	return
}
