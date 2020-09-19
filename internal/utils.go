package resttunnel

import (
	"github.com/valyala/fasthttp"
)

// IsDiscordAPIURI returns a boolean if the current URI is a discord API endpoint
func IsDiscordAPIURI(URI *fasthttp.URI) bool {
	_, ok := discordDomains[string(URI.Host())]
	return ok && string(URI.Path()[:4]) == "/api"
}
