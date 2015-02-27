package work

import (
	"fmt"
	"github.com/garyburd/redigo/redis"
	"math/rand"
)

type worker struct {
	namespace string // eg, "myapp-work"
	pool      *redis.Pool
	jobTypes  map[string]*jobType

	redisFetchScript *redis.Script
	sampler          prioritySampler

	stopChan         chan struct{}
	doneStoppingChan chan struct{}
	
	forceIterChan chan struct{}
	doneForcingIterChan chan struct{}
}

func newWorker(namespace string, pool *redis.Pool, jobTypes map[string]*jobType) *worker {
	sampler := prioritySampler{}
	for _, jt := range jobTypes {
		sampler.add(jt.Priority, redisKeyJobs(namespace, jt.Name), redisKeyJobsInProgress(namespace, jt.Name))
	}

	return &worker{
		namespace: namespace,
		pool:      pool,
		jobTypes:  jobTypes,

		redisFetchScript: redis.NewScript(len(jobTypes)*2, redisLuaRpoplpushMultiCmd),
		sampler:          sampler,

		stopChan:         make(chan struct{}),
		doneStoppingChan: make(chan struct{}),
		
		forceIterChan:         make(chan struct{}),
		doneForcingIterChan: make(chan struct{}),
	}
}

func (w *worker) start() {
	go w.loop()
}

func (w *worker) stop() {
	close(w.stopChan)
	<-w.doneStoppingChan
}

func (w *worker) forceIter() {
	w.forceIterChan <- struct{}{}
	<-w.doneForcingIterChan
}



func (w *worker) loop() {
	for {
		select {
		case <-w.stopChan:
			close(w.doneStoppingChan)
			return
		case <-w.forceIterChan:
			// forcing the worker to do some work is for testing purposes
			w.loopIteration()
			w.doneForcingIterChan <- struct{}{}
		default:
			didJob := w.loopIteration()
			if !didJob {
				// maybe sleep
			}
		}
	}
}

func (w *worker) loopIteration() bool {
	job, err := w.fetchJob()
	if err != nil {
		logError("fetch", err)
	} else if job != nil {
		w.processJob(job)
	} else {
		return false
	}
	return true
}

func (w *worker) fetchJob() (*Job, error) {
	// resort queues
	// NOTE: we could optimize this to only resort every second, or something.
	w.sampler.sample()

	var scriptArgs = make([]interface{}, 0, len(w.sampler.samples)*2)
	for _, s := range w.sampler.samples {
		scriptArgs = append(scriptArgs, s.redisJobs, s.redisJobsInProg)
	}

	conn := w.pool.Get()
	defer conn.Close()
	values, err := redis.Values(w.redisFetchScript.Do(conn, scriptArgs...))
	if err == redis.ErrNil {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	if len(values) != 3 {
		return nil, fmt.Errorf("need 3 elements back")
	}

	rawJSON, ok := values[0].([]byte)
	if !ok {
		return nil, fmt.Errorf("response msg not bytes")
	}

	dequeuedFrom, ok := values[1].([]byte)
	if !ok {
		return nil, fmt.Errorf("response queue not bytes")
	}

	inProgQueue, ok := values[2].([]byte)
	if !ok {
		return nil, fmt.Errorf("response in prog not bytes")
	}

	job, err := newJob(rawJSON, dequeuedFrom, inProgQueue)
	if err != nil {
		return nil, err
	}

	return job, nil
}

func (w *worker) processJob(job *Job) {
	defer w.removeJobFromInProgress(job)
	//fmt.Println("JOB: ", *job, string(job.dequeuedFrom))
	if jt, ok := w.jobTypes[job.Name]; ok {
		if runErr := runJob(job, jt); runErr != nil {
			job.failed(runErr)
			w.addToRetryOrDead(jt, job, runErr)
		}
	} else {
		// NOTE: since we don't have a jobType, we don't know max retries
		runErr := fmt.Errorf("stray job -- no handler")
		job.failed(runErr)
		w.addToDead(job, runErr)
		// todo: stray job?
	}
}

func (w *worker) removeJobFromInProgress(job *Job) {
	conn := w.pool.Get()
	defer conn.Close()

	_, err := conn.Do("LREM", 1, job.rawJSON)
	if err != nil {
		// todo: log error
	}
}

func (w *worker) addToRetryOrDead(jt *jobType, job *Job, runErr error) {
	failsRemaining := int64(jt.MaxFails) - job.Fails
	if failsRemaining > 0 {
		w.addToRetry(job, runErr)
	} else {
		if !jt.SkipDead {
			w.addToDead(job, runErr)
		}
	}
}

func (w *worker) addToRetry(job *Job, runErr error) {
	rawJSON, err := job.Serialize()
	if err != nil {
		// todo: log
		return
	}

	conn := w.pool.Get()
	defer conn.Close()

	_, err = conn.Do("ZADD", redisKeyRetry(w.namespace), nowEpochSeconds()+backoff(job.Fails), rawJSON)
	if err != nil {
		// todo log
	}

}

func (w *worker) addToDead(job *Job, runErr error) {
	rawJSON, err := job.Serialize()

	if err != nil {
		// todo: log
		return
	}

	conn := w.pool.Get()
	defer conn.Close()

	_, err = conn.Do("ZADD", redisKeyDead(w.namespace), nowEpochSeconds()+backoff(job.Fails), rawJSON)
	// NOTE: sidekiq limits the # of jobs: only keep jobs for 6 months, and only keep a max # of jobs
	// The max # of jobs seems really horrible. Seems like
	// conn.Send("ZREMRANGEBYSCORE", redisKeyDead(w.namespace), "-inf", now - keepInterval)
	// conn.Send("ZREMRANGEBYRANK", redisKeyDead(w.namespace), 0, -maxJobs)
	if err != nil {
		// todo log
	}
}

// backoff returns number of seconds t
func backoff(fails int64) int64 {
	return (fails * fails * fails * fails) + 15 + (rand.Int63n(30) * (fails + 1))
}
