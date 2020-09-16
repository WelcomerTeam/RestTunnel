package resttunnel

import (
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/tevino/abool"
	"github.com/valyala/fasthttp"
)

// RestTunnel represents the global application state
type RestTunnel struct {
	Logger zerolog.Logger `json:"-"`

	Start time.Time `json:"uptime"`

	HTTP *fasthttp.Client `json:"-"`

	bucketsMu sync.RWMutex
	Buckets   map[string]*Bucket `json:"buckets"`

	queuesMu sync.RWMutex
	Queues   map[string]*Queue `json:"queue"`

	callbacksMu sync.RWMutex
	Callbacks   map[uuid.UUID]*TunnelResponse `json:"callbacks"`
}

// Queue represents a Deque with a priority queue
type Queue struct {
	JobActive *abool.AtomicBool
	Bucket    *Bucket

	events   *int64
	priority *int64

	PriorityEvents chan *TunnelRequest
	Events         chan *TunnelRequest
}

// HandleQueueJob handles a TunnelRequest gathered from a queue
func (rt *RestTunnel) HandleQueueJob(tr *TunnelRequest) {

}

// StartQueueJob starts a queue job
func (rt *RestTunnel) StartQueueJob(q *Queue) {
	// If we try to start the job whilst it is already running, we will nope out of there.
	if q.JobActive.IsSet() {
		return
	}

	q.JobActive.Set()
	defer q.JobActive.UnSet()

	for {
		// Gives channel PriorityEvents priority
		select {
		case tr := <-q.PriorityEvents:
			rt.HandleQueueJob(tr)
			continue
		default:
		}
		select {
		case tr := <-q.PriorityEvents:
			rt.HandleQueueJob(tr)
			continue
		case tr := <-q.Events:
			rt.HandleQueueJob(tr)
			continue
		}
	}
}

// HandleRequest handles a HTTP request given to RestTunnel
func (rt *RestTunnel) HandleRequest(ctx *fasthttp.RequestCtx) {
	hasPriority, err := strconv.ParseBool(string(ctx.Request.Header.Peek("RT-Priority")))
	if err != nil {
		hasPriority = false
	}
	responseType, _ := ParseResponse(string(ctx.Request.Header.Peek("RT-ResponseType")))

	println(string(ctx.Response.Header.Peek("Accept-Encoding")))

	requestHeaders := fasthttp.RequestHeader{}
	ctx.Request.Header.CopyTo(&requestHeaders)

	tunnelRequest := TunnelRequest{
		ID: uuid.New(),

		ResponseType: responseType,
		Priority:     hasPriority,

		Bucket: "",

		Callback: make(chan bool),
	}

	tunnelRequest.Bucket = "A"

}

// TraverseBucket returns the origional bucket from the bucket alias.
// Returns current bucket, slice of buckets traversed through and error.
func (rt *RestTunnel) TraverseBucket(bucketStr string) (bucket *Bucket, bucketStack []string, err error) {
	stack := make(map[string]bool)
	for {
		if _, ok := stack[bucketStr]; ok {
			return nil, bucketStack, ErrBucketCircularAlias
		}

		bucketStack = append(bucketStack, bucketStr)
		stack[bucketStr] = true
		bucket, ok := rt.Buckets[bucketStr]
		if !ok {
			return nil, bucketStack, ErrBucketDoesNotExist
		}
		if bucket.alias != "" && bucket.alias != bucket.name {
			bucketStr = bucket.alias
		} else {
			break
		}
	}
	return
}

// Utilised Headers
// RestTunnel-URL: The url you are requesting
// RestTunnel-Method: the response type that is wanted (response/callback/none) (defaults to response)
// RestTunnel-Priority: boolean if the request is high priority
// Any other headers passed are sent to the request (except encoding however is still handled serverside normally so there is no difference)

// Response Headers
// RestTunnel-Timing-Request: unix of request received
// RestTunnel-Timing-Queued: unix of request added to queue/prioritised
// RestTunnel-Timing-Limited: unix of request waiting for ratelimit
// RestTunnel-Timing-Executed: unix of when request was actually executed
// RestTunnel-Timing-Completed: unix of when request was completed
// (This might be cut down into either a single int with ms to complete all or split into waiting for cache, ratelimit and execution split with ;)

// RestTunnel-UUID: UUID of Request
// RestTunnel-Ratelimit-Hit: True if a ratelimit occured
// RestTunnel-Ratelimit-Bucket: Name of the final bucket that was used
// RestTunnel-Ratelimit-Buckets: Name of all traversed buckets (seperated by ;) origional;alias;alias;final

// The process of handling a request with RestTunnel is fairly basic and is split
// into multiple sections:
// - Buckets
// - Queues
// - Callbacks

// The order and how these 3 systems are handled are completely based upon the
// ResponseType that the client initiating the request has told RestTunnel. These are:
// -  RespondWithResponse
//  When passing this type, RestTunnel will wait until the request has finished
// 	before returning a response to the client.
// -  RespondWithUUIDCallback
// 	When passing this type, RestTunnel will return a UUID to the client reguardless
// 	if the event has been processed yet.
// -  NoResponse
// 	If passed, RestTunnel will simply acknowledge the request and process it without
// 	disclosing its response or status to the client.

// Ontop of providing how the response is given to the client, the client can also
// provide if the request is high priority. By specifying true to RestTunnel-Priority,
// the execution of any other events are halted and the high priority request is
// executed beforehand. If high priority is not provided, the request will be added to
// the queue which performs as a deque.

// As this is the case, it is recommended you only use RespondwithResponse with
// priority requests as it is the case where it may have to wait for other requests
// to process first which could cause the connection to timeout.

// When priority is enabled, instead of adding the job to the Queue deque, it will
// just lock and add 1 to the Priority WaitGroup 1 which is removed when its done to
// signal the queue job to yield.

// When receiving a RespondWithResponse, it will execute a request then read from the
// TunnelRequest.Callback channel to signify that the request has been completed in
// which the TunnelRequest.Response is given to the client.

// When receiving a RespondWithUUIDCallback, it will return the TunnelRequest UUID
// to the client then execute the request seperately. It is up to the client to poll
// /callback/uuid or subscribe in gateway to retrieve the response.

// The Queue has a dedicated task which will loop until there are no more requests
// in the queue in which it will stop execution. On every loop it will first wait
// for the priority event WaitGroup which will ensure any high priority events are
// queued first.

// +---+
// |   v
// |   Wait for Priority event WaitGroup
// |   Retries the length of the queue with an RLock then RUnlocks
// |   +- Queue length is 0 -> Stop job
// |   Lock the queue, Pop a request from the queue then Unlock the queue
// |   Execute the TunnelRequest job
// |   Add the response to the struct and then add to the Callbacks table
// |   Broadcast to the TunnelRequest.Callback channel to signal RespondWithResponse
// |   +- CallbackTTL job is not running -> Start job
// +---+

// The CallbackTTL job is a loop that runs every x seconds (by default 60) to remove
// old callbacks from the cache. It is expected that if a client expects a callback
// that it is requested within 2 minutes then it is deleted. This loop checks all
// entries and will delete any keys that have expired. It will also store the time
// of the smallest time in the future that has not expired and will wait until then
// or 60 seconds (whatever is smaller).

// +---+
// |   v
// |   Set expirationCounter to 60
// |   Iterate entries
// |   +---+
// |   |   v
// |   |   +- Expired -> Mark entry for deletion -> `continue`
// |   |   +- time until < expirationCounter -> Set expirationCounter to expiration
// |   +---+                                    of entry
// |   Delete all entries marked for deletion
// |   +- Callbacks length is 0 -> Stop job
// |   Sleep for expirationCounter
// +---+
