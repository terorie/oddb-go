package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/sirupsen/logrus"
	"github.com/terorie/od-database-crawler/fasturl"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"time"
)

var activeTasksLock sync.Mutex
var activeTasks = make(map[uint64]bool)
var numActiveTasks int32
var totalQueued int64

func Schedule(c context.Context, remotes <-chan *OD) {
	go Stats(c)

	for {
		select {
		case remote := <-remotes:
			if !scheduleNewTask(c, remote) {
				return
			}
		case <-c.Done():
			return
		}
	}
}

func scheduleNewTask(c context.Context, remote *OD) bool {
	logrus.WithField("url", remote.BaseUri.String()).
		Info("Starting crawler")

	// Collect results
	results := make(chan File)

	remote.WCtx.OD = remote

	// Get queue path
	queuePath := path.Join("queue", fmt.Sprintf("%d", remote.Task.WebsiteId))

	// Delete existing queue
	if err := os.RemoveAll(queuePath);
		err != nil { panic(err) }

	// Start new queue
	var err error
	remote.WCtx.Queue, err = OpenQueue(queuePath)
	if err != nil { panic(err) }

	// Spawn workers
	remote.WCtx.SpawnWorkers(c, results, config.Workers)

	// Enqueue initial job
	atomic.AddInt32(&numActiveTasks, 1)
	remote.WCtx.queueJob(Job{
		Uri:    remote.BaseUri,
		UriStr: remote.BaseUri.String(),
		Fails:  0,
	})

	// Upload result when ready
	globalWait.Add(1)
	go remote.Watch(results)

	// Sleep if max number of tasks are active
	for atomic.LoadInt32(&numActiveTasks) > config.Tasks {
		if !sleep(time.Second, c) {
			break
		}
	}

	return true
}

func ScheduleTask(remotes chan<- *OD, t *Task, u *fasturl.URL) {
	if !t.register() {
		return
	}

	now := time.Now()
	od := &OD {
		Task: *t,
		BaseUri: *u,
		Result: TaskResult {
			WebsiteId: t.WebsiteId,
			StartTime: now,
			StartTimeUnix: now.Unix(),
		},
	}
	remotes <- od
}

func (t *Task) register() bool {
	activeTasksLock.Lock()
	defer activeTasksLock.Unlock()

	if _, known := activeTasks[t.WebsiteId]; known {
		return false
	} else {
		activeTasks[t.WebsiteId] = true
		return true
	}
}

func (t *Task) unregister() {
	activeTasksLock.Lock()
	delete(activeTasks, t.WebsiteId)
	activeTasksLock.Unlock()
}

func (o *OD) Watch(results chan File) {
	// Mark job as completely done
	defer globalWait.Done()
	defer o.Task.unregister()

	filePath := path.Join("crawled", fmt.Sprintf("%d.json", o.Task.WebsiteId))

	// Open crawl results file
	f, err := os.OpenFile(
		filePath,
		os.O_CREATE | os.O_RDWR | os.O_APPEND,
		0644,
	)
	if err != nil {
		logrus.WithError(err).
			Error("Failed saving crawl results")
		return
	}
	defer f.Close()
	defer os.Remove(filePath)

	// Listen for exit code of Collect()
	collectErrC := make(chan error)

	// Block until all results are written
	// (closes results channel)
	if !o.handleCollect(results, f, collectErrC) {
		return
	}

	// Exit code of Collect()
	err = <-collectErrC
	close(collectErrC)
	if err != nil {
		logrus.WithError(err).
			Error("Failed saving crawl results")
		return
	}

	// Upload results
	err = PushResult(&o.Result, f)
	if err != nil {
		logrus.WithError(err).
			Error("Failed uploading crawl results")
		return
	}
}

// Returns if aborted naturally (results ready for upload)
func (o *OD) handleCollect(results chan File, f *os.File, collectErrC chan error) bool {
	// Begin collecting results
	go o.Task.Collect(results, f, collectErrC)
	defer close(results)

	// Wait for all jobs on remote to finish
	for {
		// Natural finish
		if atomic.LoadInt64(&o.InProgress) == 0 {
			o.onTaskFinished()
			return true
		}
		// Abort
		if atomic.LoadInt32(&o.WCtx.aborted) != 0 {
			// Wait for all workers to finish
			o.WCtx.workers.Wait()
			o.onTaskPaused()
			return false
		}

		time.Sleep(500 * time.Millisecond)
	}
}

func (o *OD) onTaskFinished() {
	defer atomic.AddInt32(&numActiveTasks, -1)

	// Close queue
	if err := o.WCtx.Queue.Close(); err != nil {
		panic(err)
	}

	// Log finish
	logrus.WithFields(logrus.Fields{
		"id":  o.Task.WebsiteId,
		"url": o.BaseUri.String(),
		"duration": time.Since(o.Result.StartTime),
	}).Info("Crawler finished")

	// Set status code
	now := time.Now()
	o.Result.EndTimeUnix = now.Unix()
	fileCount := atomic.LoadUint64(&o.Result.FileCount)
	if fileCount == 0 {
		errorCount := atomic.LoadUint64(&o.Result.ErrorCount)
		if errorCount == 0 {
			o.Result.StatusCode = "empty"
		} else {
			o.Result.StatusCode = "directory listing failed"
		}
	} else {
		o.Result.StatusCode = "success"
	}
}

func (o *OD) onTaskPaused() {
	defer atomic.AddInt32(&numActiveTasks, -1)

	// Close queue
	if err := o.WCtx.Queue.Close(); err != nil {
		panic(err)
	}

	// Set current end time
	o.Result.EndTimeUnix = time.Now().Unix()

	// Save task metadata
	err := SaveTask(o)
	if err != nil {
		// Log finish
		logrus.WithFields(logrus.Fields{
			"err": err.Error(),
			"id":  o.Task.WebsiteId,
			"url": o.BaseUri.String(),
		}).Error("Failed to save crawler state")
		return
	}

	// Log finish
	logrus.WithFields(logrus.Fields{
		"id":  o.Task.WebsiteId,
		"url": o.BaseUri.String(),
		"duration": time.Since(o.Result.StartTime),
	}).Info("Crawler paused")
}

func (t *Task) Collect(results chan File, f *os.File, errC chan<- error) {
	err := t.collect(results, f)
	if err != nil {
		logrus.WithError(err).
			Error("Failed saving crawl results")
	}
	errC <- err
}

func (t *Task) collect(results chan File, f *os.File) error {
	for result := range results {
		result.Path = fasturl.PathUnescape(result.Path)
		result.Name = fasturl.PathUnescape(result.Name)
		resJson, err := json.Marshal(result)
		if err != nil { panic(err) }
		_, err = f.Write(resJson)
		if err != nil { return err }
		_, err = f.Write([]byte{'\n'})
		if err != nil { return err }
	}

	return nil
}
