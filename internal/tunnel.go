package resttunnel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
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

// CallbackExpiration is the duration a callback can exist before it is treated as stale and removed
const CallbackExpiration = time.Minute * 2

var json = jsoniter.ConfigCompatibleWithStandardLibrary

var discordDomains = map[string]bool{
	"discord.com":    true,
	"discordapp.com": true,
}

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

	ctx, cancel := context.WithCancel(context.Background())
	rt = &RestTunnel{
		ctx:    ctx,
		cancel: cancel,

		Logger: zerolog.New(logger).With().Timestamp().Logger(),
		Start:  time.Now().UTC(),

		HTTP: &fasthttp.Client{
			Name:                     "RestTunnel",
			NoDefaultUserAgentHeader: true,
			MaxIdleConnDuration:      time.Second * 5,
			MaxConnDuration:          time.Second * 5,
			ReadTimeout:              time.Second * 5,
			WriteTimeout:             time.Second * 5,
		},
		bucketsMu: sync.RWMutex{},
		Buckets:   make(map[string]*Bucket),

		queuesMu: sync.RWMutex{},
		Queues:   make(map[string]*Queue),

		callbacksMu: sync.RWMutex{},
		Callbacks:   make(map[uuid.UUID]*TunnelResponse),
	}
	return
}

// HandleQueueJob handles a TunnelRequest gathered from a queue
func (rt *RestTunnel) HandleQueueJob(tr *TunnelRequest) {

	// This goroutine shows a job that is taking too long.
	stage := "init"
	fin := make(chan bool)
	go func() {
		waitTime := time.Second * 5
		since := time.Now()
		t := time.NewTimer(waitTime)
		for {
			select {
			case <-fin:
				return
			case <-t.C:
				rt.Logger.Warn().Str("stage", stage).Msgf("Job for %s has been in stage '%s' for too long. Been executing for %f seconds. Possible deadlock?", string(tr.URI), stage, time.Now().Sub(since).Round(time.Second).Seconds())
				t.Reset(waitTime)
			}
		}
	}()
	defer func() {
		close(fin)
	}()

	stage = "aqrq"
	req := fasthttp.AcquireRequest()
	req.SetRequestURIBytes(tr.URI)

	stage = "hdrs"
	tr.Headers.VisitAll(func(key []byte, value []byte) {
		req.Header.SetBytesKV(key, value)
	})

	// Asks discord to send higher precision reset times
	req.Header.Set("X-RateLimit-Precision", "millisecond")

	var resp *fasthttp.Response
	var hit bool
	var err error

	rt.bucketsMu.RLock()
	bucket := rt.Buckets[tr.Bucket]
	var globalBucket *Bucket
	if bucket.global != "" {
		globalBucket = rt.Buckets[bucket.global]
	}
	rt.bucketsMu.RUnlock()

	for {
		stage = "bckt"

		hit = bucket.Lock()
		if bucket.global != "" {
			hit = globalBucket.Lock() || hit
		}

		stage = "aqrs"
		resp = fasthttp.AcquireResponse()

		stage = "dorq"
		err = rt.HTTP.Do(req, resp)

		println(resp.StatusCode())
		stage = "rtlm"
		if resp.StatusCode() == 429 {
			// ratelimit
			global, err := strconv.ParseBool(string(resp.Header.Peek("X-RateLimit-Global")))
			if err == nil && global {
				// Global
				rt.Logger.Warn().Msg("Got global ratelimit")
				if bucket.global != "" {
					retryAfter, err := strconv.Atoi(string(resp.Header.Peek("Retry-After")))
					if err != nil {
						// Convert ms to ns (x1e6)
						globalBucket.Exhaust(time.Now().UnixNano() + int64(retryAfter*1000000))
					}
				} else {
					rt.Logger.Warn().Str("bucket", bucket.name).Msg("Got global ratelimit but i dont have a global bucket set.")
				}
			} else {
				// We ignore X-RateLimit-Reset as we already have Reset-Afdter
				rt.Logger.Warn().Msg("Got endpoint ratelimit")
				rlLimit, err := strconv.Atoi(string(resp.Header.Peek("X-RateLimit-Limit")))
				if err != nil {
					rt.Logger.Warn().Msgf("Failed to convert X-RateLimit-Limit '%s' to int", string(resp.Header.Peek("X-RateLimit-Limit")))
				}
				rlResetRaw, err := strconv.ParseFloat(string(resp.Header.Peek("X-RateLimit-Reset-After")), 64)
				if err != nil {
					rt.Logger.Warn().Msgf("Failed to convert X-RateLimit-Reset-After '%s' to float", string(resp.Header.Peek("X-RateLimit-Reset-After")))
				}
				rlBucket := string(resp.Header.Peek("X-RateLimit-Bucket"))
				// If we have received 429 we already know it is 0
				// rlRemaining, err := strconv.Atoi(string(resp.Header.Peek("X-RateLimit-Remaining")))
				// if err != nil {
				// 	rt.Logger.Warn().Msgf("Failed to convert X-RateLimit-Remaining '%s' to int", string(resp.Header.Peek("X-RateLimit-Remaining")))
				// }

				// Convert seconds to nanoseconds (x1e9) (we add on about 500ms just incase)
				rlReset := time.Now().UnixNano() + int64(rlResetRaw*float64(1000000000)) + 500000000
				bucket.Exhaust(rlReset)
				bucket.Modify(int32(rlLimit), atomic.LoadInt64(bucket.duration), rlBucket, bucket.global)
			}
		} else {
			rlBucket := string(resp.Header.Peek("X-RateLimit-Bucket"))
			rt.Logger.Debug().Msgf("Received bucket '%s'", rlBucket)
			if rlBucket != bucket.alias && rlBucket != "" {
				rt.Logger.Info().Msgf("Discovered alias bucket '%s' for '%s'", rlBucket, bucket.name)
				bucket.Modify(
					atomic.LoadInt32(bucket.limit),
					atomic.LoadInt64(bucket.duration),
					rlBucket,
					bucket.global,
				)
			}

			rlLimit, err := strconv.Atoi(string(resp.Header.Peek("X-RateLimit-Limit")))
			if err != nil {
				rt.Logger.Warn().Msgf("Failed to convert X-RateLimit-Limit '%s' to int", string(resp.Header.Peek("X-RateLimit-Limit")))
			}
			rlRemaining, err := strconv.Atoi(string(resp.Header.Peek("X-RateLimit-Remaining")))
			if err != nil {
				rt.Logger.Warn().Msgf("Failed to convert X-RateLimit-Remaining '%s' to int", string(resp.Header.Peek("X-RateLimit-Remaining")))
			}
			rlReset, err := strconv.ParseFloat(string(resp.Header.Peek("X-RateLimit-Reset-After")), 64)
			if err != nil {
				rt.Logger.Warn().Msgf("Failed to convert X-RateLimit-Reset-After '%s' to float", string(resp.Header.Peek("X-RateLimit-Reset-After")))
			}

			if rlRemaining == rlLimit-1 {
				// We have just received a new RateLimit so we can now infer the ratelimit
				bucket.Modify(int32(rlLimit), time.Duration(rlReset*1000000000).Nanoseconds(), rlBucket, bucket.global)
			}

			break
		}
	}

	tunnelResponse := &TunnelResponse{
		expiration: time.Now().UTC().Add(CallbackExpiration),

		ID:           tr.ID,
		RatelimitHit: hit,
		Response:     resp,
		Error:        err,
	}

	stage = "clbk"
	rt.callbacksMu.Lock()
	rt.Callbacks[tr.ID] = tunnelResponse
	rt.callbacksMu.Unlock()

	stage = "chan"
	if tr.ResponseType == RepondWithResponse {
		tr.Callback <- true
	}
	stage = "done"
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
		atomic.AddInt64(q.TotalJobsActive, -1)
	}()

	for {
		timeout := time.NewTicker(time.Second * 5)
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
		case <-timeout.C:
			return
		case <-q.JobClosure:
			return
		}
	}
}

// BalanceQueueJobs is the task that ensures that the queue is able to keep up.
// This might not even be used as a QueueJob is made for each endpoint so its
// unlikely a single endpoint will get overwhelmed.
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

		rt.SetQueueCount(q, actualJobsNeeded)
	}
}

// HandleRequest handles a HTTP request given to RestTunnel
func (rt *RestTunnel) HandleRequest(ctx *fasthttp.RequestCtx) {
	startTime := time.Now().UTC()
	defer func() {
		rt.Logger.Debug().Msgf("Finished request in %d ms", time.Now().UTC().Sub(startTime).Milliseconds())
	}()

	id := uuid.New()

	hasPriority, err := strconv.ParseBool(string(ctx.Request.Header.Peek("RT-Priority")))
	if err != nil {
		hasPriority = false
	}
	responseType, _ := ParseResponse(string(ctx.Request.Header.Peek("RT-ResponseType")))

	requestURI := ctx.Request.Header.Peek("RT-URL")
	if len(requestURI) == 0 {
		ctx.SetStatusCode(400)
		return
	}

	URI := fasthttp.AcquireURI()
	err = URI.Parse(nil, requestURI)
	if err != nil {
		ctx.SetStatusCode(400)
		return
	}

	rt.Logger.Debug().Str("response", responseType.String()).Bool("priority", hasPriority).Msg("Received request")

	requestHeaders := fasthttp.RequestHeader{}
	ctx.Request.Header.CopyTo(&requestHeaders)

	requestHeaders.Set("Accept-Encoding", "gzip, deflate, br")
	requestHeaders.Set("User-Agent", fmt.Sprintf("restTunnel/%s; %s", VERSION, string(requestHeaders.Peek("User-Agent"))))

	// Create a bucket with <HOST>:<PATH + AUTHORIZATION with SHA256>
	pathHash := sha256.New()
	pathHash.Write(URI.Path()[:])
	pathHash.Write(requestHeaders.Peek("Authorization"))
	initialBucketName := string(URI.Host()) + ":" + hex.EncodeToString(pathHash.Sum(nil))

	// We then traverse the bucket incase it does not exist or it has an alias as some paths may use the same bucket
	bucket, bucketStack, err := rt.TraverseBucket(initialBucketName)

	if err != nil {
		rt.Logger.Debug().Err(err).Msgf("Bucket '%s' does not exist yet", initialBucketName)
	} else {
		rt.Logger.Debug().Str("bucket", bucket.name).Msgf("Traversed bucket: %v", bucketStack)
	}

	var bucketName string
	var queue *Queue

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
		bucketName = initialBucketName
	default:
		bucketName = bucket.name
	}

	globalBucket := ""

	// We will create a global ratelimiter when we attempt to use a discord API
	if IsDiscordAPIURI(URI) {
		pathHash := sha256.New()
		pathHash.Write(requestHeaders.Peek("Authorization"))
		globalBucket = hex.EncodeToString(pathHash.Sum(nil))
	}

	// Create bucket if it does not exist
	rt.bucketsMu.Lock()
	if _, ok := rt.Buckets[bucketName]; !ok {
		rt.Logger.Debug().Msgf("Creating bucket '%s'", bucketName)
		bucket = CreateBucket(bucketName, 5, time.Second*5, "", globalBucket)
		rt.Buckets[bucketName] = bucket
	}
	if _, ok := rt.Buckets[globalBucket]; !ok {
		rt.Logger.Debug().Msgf("Creating global bucket '%s'", globalBucket)
		bucket = CreateBucket(globalBucket, 50, time.Second, "", "")
		rt.Buckets[globalBucket] = bucket
	}
	rt.bucketsMu.Unlock()

	// Initialize queue if it does not exist
	rt.queuesMu.Lock()
	if _, ok := rt.Queues[bucketName]; !ok {
		rt.Logger.Debug().Msgf("Creating queue '%s'", bucketName)
		queue = &Queue{
			BalancerActive:  abool.New(),
			TotalJobsActive: new(int64),
			JobsActive:      &sync.WaitGroup{},
			JobClosure:      make(chan bool),
			JobsHandled:     new(int64),
			Bucket:          bucket,
			events:          new(int64),
			priority:        new(int64),
			PriorityEvents:  make(chan *TunnelRequest, 64),
			Events:          make(chan *TunnelRequest, 64),
		}
		rt.Queues[bucketName] = queue
	} else {
		queue = rt.Queues[bucketName]
	}
	rt.queuesMu.Unlock()

	tunnelRequest := TunnelRequest{
		ID: id,

		URI:     URI.FullURI(),
		Headers: &requestHeaders,

		ResponseType: responseType,
		Priority:     hasPriority,

		Bucket:   bucketName,
		Callback: make(chan bool),
	}

	// If the queue we have added to has no jobs running, start a job
	if totalJobs := atomic.LoadInt64(queue.TotalJobsActive); totalJobs == 0 {
		rt.Logger.Debug().Msgf("Creating job for queue '%s'", bucketName)
		go rt.StartQueueJob(queue)
	}

	queueEvent := func() {
		if hasPriority {
			queue.PriorityEvents <- &tunnelRequest
		} else {
			queue.Events <- &tunnelRequest
		}
	}

	if responseType == NoResponse {
		go queueEvent()
		return
	}

	queueEvent()

	// RestTunnel-UUID: UUID of Request
	// RestTunnel-Ratelimit-Hit: True if a ratelimit occured
	// RestTunnel-Ratelimit-Bucket: Name of the final bucket that was used
	// RestTunnel-Ratelimit-Buckets: Name of all traversed buckets (seperated by ;) origional;alias;alias;final

	ctx.Response.Header.Set("RT-Hit", "false")

	switch responseType {
	case RepondWithResponse:
		// Wait for the callback channel to say the request has been completed then
		// retrieve from callbacks and remove.
		rt.Logger.Debug().Msg("Waiting for callback")
		<-tunnelRequest.Callback
		rt.Logger.Debug().Msg("Received callback")

		rt.callbacksMu.RLock()
		tunnelResponse, ok := rt.Callbacks[tunnelRequest.ID]
		close(tunnelRequest.Callback)
		delete(rt.Callbacks, tunnelRequest.ID)
		rt.callbacksMu.RUnlock()

		if !ok {
			rt.Logger.Warn().Msg("Received callback but not in callback table. Ignoring...")
		} else {
			ctx.Response.Header.Set("RT-Hit", strconv.FormatBool(tunnelResponse.RatelimitHit))

			tunnelResponse.Response.Header.VisitAll(func(key []byte, value []byte) {
				ctx.Response.Header.SetBytesKV(key, value)
			})
			ctx.Response.ResetBody()

			body, _ := rt.DecodeBody(tunnelResponse.Response)

			ctx.Write(body)
			ctx.SetStatusCode(tunnelResponse.Response.StatusCode())
		}
	case RespondWithUUIDCallback:
		// We do not need to do anything as it is automatically added to the callbacks.
		// Return redirect to callback page
		ctx.Redirect("/callbacks/"+tunnelRequest.ID.String(), 303)
	}

	ctx.Response.Header.Set("RT-UUID", tunnelRequest.ID.String())
	ctx.Response.Header.Set("RT-Bucket", tunnelRequest.Bucket)
	ctx.Response.Header.Set("RT-Buckets", strings.Join(bucketStack, ";"))

	return
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
			return bucket, bucketStack, nil
		}
	}
}

// DecodeBody returns a decoded fasthttp.Responce body using the
// Content-Encoding header
func (rt *RestTunnel) DecodeBody(resp *fasthttp.Response) (body []byte, err error) {
	contentEncoding := string(resp.Header.Peek("Content-Encoding"))
	body = resp.Body()

	if contentEncoding == "" {
		return body, nil
	}

	// Decode the body using the Content-Encoding header
	for _, encoding := range strings.Split(contentEncoding, ";") {
		switch encoding {
		case "gzip":
			body, err = resp.BodyUnbrotli()
			if err != nil {
				rt.Logger.Error().Err(err).Msg("Failed to Gunzip body")
			}
			continue
		case "deflate":
			body, err = resp.BodyInflate()
			if err != nil {
				rt.Logger.Error().Err(err).Msg("Failed to Inflate body")
			}
			continue
		case "br":
			body, err = resp.BodyUnbrotli()
			if err != nil {
				rt.Logger.Error().Err(err).Msg("Failed to Unbrotli body")
			}
			continue
		default:
			rt.Logger.Warn().Msgf("Unexpected encoding '%s'", encoding)
		}
	}
	return body, err
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
