package resttunnel

import (
	"math"
	"math/rand"
	"strconv"
	"time"

	accumulator "github.com/TheRockettek/RestTunnel/pkg/accumulator"
	structs "github.com/TheRockettek/RestTunnel/structs"
	"github.com/google/uuid"
	"github.com/savsgio/gotils"
	"github.com/valyala/fasthttp"
)

const (
	daySeconds    = 86400
	hourSeconds   = 3600
	minuteSeconds = 60
)

// IsDiscordAPIURI returns a boolean if the current URI is a discord API endpoint.
func IsDiscordAPIURI(uri *fasthttp.URI) bool {
	return gotils.StringSliceInclude(discordDomains, gotils.B2S(uri.Path()[:4]))
}

// DurationTimestamp outputs in a format similar to the timestamp String().
func DurationTimestamp(d time.Duration) (output string) {
	seconds := d.Seconds()
	if seconds > daySeconds {
		days := math.Trunc(seconds / daySeconds)
		if days > 0 {
			output += strconv.Itoa(int(days)) + "d"
		}

		seconds = math.Mod(seconds, daySeconds)
	}

	if seconds > hourSeconds {
		hours := math.Trunc(seconds / hourSeconds)
		if hours > 0 {
			output += strconv.Itoa(int(hours)) + "h"
		}

		seconds = math.Mod(seconds, hourSeconds)
	}

	minutes := math.Trunc(seconds / minuteSeconds)
	if minutes > 0 {
		output += strconv.Itoa(int(minutes)) + "m"
	}

	seconds = math.Mod(seconds, minuteSeconds)
	if seconds > 0 {
		output += strconv.Itoa(int(seconds)) + "s"
	}

	return output
}

// createLineChart creates a LineChart from an accumulator.
func createLineChart(ac *accumulator.Accumulator,
	background string, border string) (chart structs.LineChart) {
	data := make([]interface{}, 0, len(ac.Samples))

	for _, sample := range ac.Samples {
		data = append(data, structs.DataStamp{
			Time:  sample.StoredAt,
			Value: sample.Value,
		})
	}

	chart = structs.LineChart{
		Datasets: []structs.Dataset{{
			Label:            ac.Label,
			BackgroundColour: background,
			BorderColour:     border,
			Data:             data,
		}},
	}

	return chart
}

// randomCallback returns a random callback from callbacks.
func (rt *RestTunnel) randomCallback() (k uuid.UUID, v *structs.TunnelResponse) {
	rt.callbacksMu.RLock()
	defer rt.callbacksMu.RUnlock()

	if len(rt.Callbacks) < 1 {
		return
	}

	i := rand.Intn(len(rt.Callbacks)) // nolint:gosec
	for k, v = range rt.Callbacks {
		if i == 0 {
			return k, v
		}
		i--
	}

	return
}
