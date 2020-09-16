package resttunnel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	jsoniter "github.com/json-iterator/go"
	"github.com/rs/zerolog"
	"github.com/tevino/abool"
	"github.com/valyala/fasthttp"
)

// VERSION respects semantic versioning
const VERSION = "0.1"

var json = jsoniter.ConfigCompatibleWithStandardLibrary

// TunnelConfiguration represents the configuration for RestTunnel
type TunnelConfiguration struct {
	Host string `json:"host" yaml:"host"`

	Logging struct {
		ConsoleLoggingEnabled bool `json:"console_logging" yaml:"console_logging"`
		FileLoggingEnabled    bool `json:"file_logging" yaml:"file_logging"`

		EncodeAsJSON bool `json:"encode_as_json" yaml:"encode_as_json"` // Make the framework log as json

		Directory  string `json:"directory" yaml:"directory"`     // Directory to log into
		Filename   string `json:"filename" yaml:"filename"`       // Name of logfile
		MaxSize    int    `json:"max_size" yaml:"max_size"`       /// Size in MB before a new file
		MaxBackups int    `json:"max_backups" yaml:"max_backups"` // Number of files to keep
		MaxAge     int    `json:"max_age" yaml:"max_age"`         // Number of days to keep a logfile
	} `json:"logging" yaml:"logging"`
}

// RestTunnel represents the global application state
type RestTunnel struct {
	ctx    context.Context
	cancel func()

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
	// We use a channel to painlessly control how many Jobs should be active. If we have 4
	// jobs active and only want 2 running, we send 2 booleans down JobClosure and if we
	// wanted to add 1 more we would just create a Queue job normally. Reference
	// TotalJobsActive for how many are active and use JobsActive to wait when closing
	// a queue. BalancerActive signifys if the balancer is enabled and limits to only one
	// being ran at a time. The balancer simply creates more queue jobs when necessary.
	BalancerActive *abool.AtomicBool

	TotalJobsActive *int64
	JobsActive      *sync.WaitGroup
	JobClosure      chan bool

	JobsHandled *int64

	Bucket *Bucket

	events   *int64
	priority *int64

	PriorityEvents chan *TunnelRequest
	Events         chan *TunnelRequest
}

// ErrorResponse is the structure error messages are returned to by the client. We do not
// have a Success response as the raw response is sent.
type ErrorResponse struct {
	Error   string    `json:"error"`
	Success bool      `json:"success"`
	Queued  bool      `json:"queued"` // Boolean if the request has already been queued
	UUID    uuid.UUID `json:"uuid"`
}

// NewTunnel creates a RestTunnel instance
func NewTunnel(logger io.Writer) (rt *RestTunnel, err error) {

	rt = &RestTunnel{
		Logger: zerolog.New(logger).With().Timestamp().Logger(),
		Start:  time.Now().UTC(),

		HTTP: &fasthttp.Client{
			Name:                          "RestTunnel",
			NoDefaultUserAgentHeader:      true,
			DisableHeaderNamesNormalizing: true,
		},

		bucketsMu: sync.RWMutex{},
		Buckets:   make(map[string]*Bucket),

		queuesMu: sync.RWMutex{},
		Queues:   make(map[string]*Queue),

		callbacksMu: sync.RWMutex{},
		Callbacks:   make(map[string]*TunnelResponse),
	}
}

// HandleQueueJob handles a TunnelRequest gathered from a queue
func (rt *RestTunnel) HandleQueueJob(tr *TunnelRequest) {
}

// SetQueueCount changes how many queue jobs are running
func (rt *RestTunnel) SetQueueCount(q *Queue, count int64) {
	change := count - atomic.LoadInt64(q.TotalJobsActive)

	if change == 0 {
		return
	}

	if change > 0 {
		// Create more jobs
		for i := int64(0); i < change; i++ {
			go rt.StartQueueJob(q)
		}
	} else {
		// Send JobClosure events to stop a single running task
		for i := int64(0); i < change; i++ {
			q.JobClosure <- true
		}
	}
}

// StartQueueJob starts a queue job
func (rt *RestTunnel) StartQueueJob(q *Queue) {
	atomic.AddInt64(q.TotalJobsActive, 1)
	q.JobsActive.Add(1)
	defer func() {
		q.JobsActive.Done()
		atomic.AddInt64(q.TotalJobsActive, 1)
	}()

	for {
		// Gives channel PriorityEvents priority
		select {
		case tr := <-q.PriorityEvents:
			atomic.AddInt64(q.JobsHandled, 1)
			rt.HandleQueueJob(tr)
			continue
		default:
		}
		select {
		case tr := <-q.PriorityEvents:
			atomic.AddInt64(q.JobsHandled, 1)
			rt.HandleQueueJob(tr)
			continue
		case tr := <-q.Events:
			atomic.AddInt64(q.JobsHandled, 1)
			rt.HandleQueueJob(tr)
			continue
		case <-q.JobClosure:
			return
		}
	}
}

// BalanceQueueJobs is the task that ensures that the queue is able to keep up
func (rt *RestTunnel) BalanceQueueJobs(q *Queue) {
	// Duration how long we should account for
	preserveTime := time.Minute
	// How often we should check
	interval := 30 * time.Second

	// Minimum a job can do on its own. If we try launch 10 jobs when we only
	// process 40 requests, it will limit to only 2 because the minEventsPerJob
	// is 30. Increase to make it less likely jobs will be removed.
	minEventsPerJob := 30

	events := int64(0)

	// Used to extrapolate how many jobs should be running
	timeScale := float64(preserveTime) / float64(interval)

	// Initial
	rt.SetQueueCount(q, 1)

	t := time.NewTicker(interval)
	for {
		<-t.C

		jobsSince := atomic.LoadInt64(q.JobsHandled)
		change := jobsSince - events

		minuteHandle := float64(change) * timeScale
		waitingOn := float64(len(q.PriorityEvents) + len(q.Events))
		jobScale := (minuteHandle + waitingOn) / minuteHandle
		jobsNeeded := math.Floor(float64(atomic.LoadInt64(q.TotalJobsActive)) * jobScale)

		actualJobsNeeded := int64(math.Min(jobsNeeded, math.Floor(minuteHandle/float64(minEventsPerJob))))

		println("Handling", minuteHandle)
		println("Waiting On", waitingOn)
		println("Expected jobs needed", jobsNeeded, "scale of", jobScale)
		println("Minimum", math.Floor(minuteHandle/float64(minEventsPerJob)))
		println("Chose", actualJobsNeeded)
	}
}

// HandleRequest handles a HTTP request given to RestTunnel
func (rt *RestTunnel) HandleRequest(ctx *fasthttp.RequestCtx) {
	id := uuid.New()

	hasPriority, err := strconv.ParseBool(string(ctx.Request.Header.Peek("RT-Priority")))
	if err != nil {
		hasPriority = false
	}
	responseType, _ := ParseResponse(string(ctx.Request.Header.Peek("RT-ResponseType")))

	requestHeaders := fasthttp.RequestHeader{}
	ctx.Request.Header.CopyTo(&requestHeaders)

	requestHeaders.Set("Accept-Encoding", "gzip, deflate, br")
	requestHeaders.Set("User-Agent", fmt.Sprintf("restTunnel/%s; %s", VERSION, string(requestHeaders.Peek("User-Agent"))))

	// Create a bucket with <HOST>:<PATH + AUTHORIZATION with SHA256>
	URI := ctx.Request.URI()
	pathHash := sha256.New()
	pathHash.Write(URI.Path()[:])
	pathHash.Write(requestHeaders.Peek("Authorization"))
	initialBucketName := string(URI.Host()) + ":" + hex.EncodeToString(pathHash.Sum(nil))

	// We then traverse the bucket incase it does not exist or it has an alias as some paths may use the same bucket
	bucket, bucketStack, err := rt.TraverseBucket(initialBucketName)

	var bucketName string
	switch err {
	case ErrBucketCircularAlias:
		ctx.SetStatusCode(409)
		res, err := json.Marshal(ErrorResponse{
			Error:   fmt.Sprintf(err.Error(), initialBucketName, bucketStack),
			Success: false,
			Queued:  false,
			UUID:    id,
		})
		if err != nil {
			ctx.Write(res)
		}
		return
	case ErrBucketDoesNotExist:
		// Bucket will be created when request is completed so signify by leaving it blank
		bucketName = ""
	default:
		bucketName = bucket.name
	}

	tunnelRequest := TunnelRequest{
		ID: id,

		URI:     ctx.Request.RequestURI(),
		Headers: &requestHeaders,

		ResponseType: responseType,
		Priority:     hasPriority,

		Bucket: bucketName,

		Callback: make(chan bool),
	}

	tunnelRequest.Bucket = "A"

}

// TraverseBucket returns the origional bucket from the bucket alias.
// Returns current bucket, slice of buckets traversed through and error.
func (rt *RestTunnel) TraverseBucket(bucketStr string) (bucket *Bucket, bucketStack []string, err error) {
	rt.bucketsMu.RLock()
	defer rt.bucketsMu.RUnlock()

	stack := make(map[string]bool)
	for {
		bucketStack = append(bucketStack, bucketStr)
		if _, ok := stack[bucketStr]; ok {
			return nil, bucketStack, ErrBucketCircularAlias
		}
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
