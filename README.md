# Routes

### Discord RateLimiting
RestTunnel was built to be able to handle discord's ratelimit system and may not be able to fully support other websites ratelimiting measures as they are all custom built. If the endpoint supplies X-RateLimit-Limit, X-RateLimit-Reset-After, X-RateLimit-Bucket, X-RateLimit-Reset and allows for millisecond values for these or through setting "millisecond" on "X-RateLimit-Precision", then they should theoretically work.

RestTunnel supports multiple bot tokens to be passed at the same time as it uses a hash of Authorization in order to differentiate specific buckets on the same endpoint.

### Endpoints
RestTunnel will reverse-proxy any request that is not one of the URIs below and will treat as a regular RestTunnel request.

/resttunnel - Endpoint to get general information about RestTunnel.
```{"success":true,"data":{"name":"RestTunnel","version":"1.1","reverse":true}}```

/resttunnel/analytics - Analytics for all the different datasets collected (RateLimit Hits, RateLimit Misses, Queued Requests, Total Requests, Callbacks in Buffer, Average Response Time). Also contains counters for the sum of these and a string version of the uptime. This endpoint was mainly created to accompany Sandwich-Daemon which will show these statistics on the dashboard when RestTunnel is connected. **The chart datasets were made to be passed directly into chart-data for chart.js**,
```{"success":true,"data":{"uptime":"2d4h49m17s","charts":{"hits":{"datasets":[{"label":"Ratelimit Hits","backgroundColor":"rgba(149, 165, 165, 0.5)","borderColor":"#7E8C8D","data":[{"x":"2021-01-17T12:43:00Z","y":0}]}]}}}}```

/resttunnel/callbacks/<id> - Endpoint used to retrieve callbacks responses, if successful will display a normal HTTP response.
```{"error":"Invalid UUID passed","success":false}```

### Sending RestTunnel requests
When passing a RestTunnel request, there are various options than you can specify which will affect when and how you receive your response. Most of these options are specified within headers and are completely optional, in most cases.

 - **RT-Priority** - Boolean if the request has priority. If a request has priority and is added to a queue, priority requests are always handled first. This ensures specific messages are sent as soon as possible in the event of a back log.
- **RT-URL** - If you are not using reverse_route, you must specify this header. This simply is the exact URL you want to send a request to. For example, I would have to set the RT-URL header to "https://discordapp.com/api/gateway" instead of a GET request to /api/gateway, if I had disabled reverse_route. This also will overwrite the URL if you are using reverse route so you do not have to run 2 instances and can defer to a specific host if you wish.
- **RT-ResponseType** - ResponseType specifies how you would like your response payload to be. There are 3 options which are:
	- RespondWithResponse - This will act similar to a normal HTTP request where it will immediately send the payload back after it has completed.
	- RespondWithUUIDCallback - This will not return the HTTP request and instead will send a redirect request to the /resttunnel/callbacks/... endpoint which will respond once the HTTP request has completed. A benefit over RespondWithResponse is the request will be queued up and you can view the call-back at a later point in time instead of immediately. **The UUID is available in the RT-UUID response header**.
	- NoResponse - Absolutely nothing is returned in response. Useful when you just want to send a request and do not need anything back. Useful when you want to send a bunch of messages at once that you do not need a response for and you do not have to wait for all requests to finish before continuing. Useful for webhooks you know are going to work.


### Response Codes
When sending requests, if any errors occur the header **RT-Error** will specify you with most details on what is wrong however there are also a few HTTP error codes that can give some insight into what has happened.
- 400 will be raised along with "Missing URL" when you have not specified a URL with reverse route disabled.
- 400 will be raised along with "Invalid URL" when you have specified an invalid URL
- 409 will be raised when a circular reference on buckets have been found. Unlikely, but if it happens on an endpoint, you're screwed.
- Any other status codes will be from the URL you specified.

### Response Headers
- **RT-Hit** - Boolean value indicating if a ratelimit was Hit or Missed
- **RT-UUID** - UUID of the request you sent.
- **RT-Bucket** - Name of last bucket related to the endpoint used
- **RT-Buckets** - Semicolon delimited list of all buckets that referenced each other