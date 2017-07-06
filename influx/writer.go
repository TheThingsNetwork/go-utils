// Copyright © 2017 The Things Network
// Use of this source code is governed by the MIT license that can be found in the LICENSE file.

package influx

import (
	"fmt"
	"sync"
	"time"

	ttnlog "github.com/TheThingsNetwork/go-utils/log"
	influxdb "github.com/influxdata/influxdb/client/v2"
)

// DefaultScalingInterval represents default scaling interval(time to wait before a new writer is started/killed) used by batching writer.
const DefaultScalingInterval = 500 * time.Millisecond

// newBatchPoints creates new influxdb.BatchPoints with specified bpConf.
// Panics on errors.
func newBatchPoints(bpConf influxdb.BatchPointsConfig) influxdb.BatchPoints {
	bp, err := influxdb.NewBatchPoints(bpConf)
	if err != nil {
		// Can only happen if there's an error in the code
		panic(fmt.Errorf("Invalid batch point configuration: %s", err))
	}
	return bp
}

// BatchPointsWriter writes influxdb.BatchPoints to Influx database.
type BatchPointsWriter interface {
	Write(bp influxdb.BatchPoints) error
}

// PointWriter writes *influxdb.Point to Influx database.
type PointWriter interface {
	Write(bpConf influxdb.BatchPointsConfig, p *influxdb.Point) error
}

// SinglePointWriter is a PointWriter, which writes points one-by-one
type SinglePointWriter struct {
	log    ttnlog.Interface
	writer BatchPointsWriter
}

// NewSinglePointWriter creates new SinglePointWriter
func NewSinglePointWriter(log ttnlog.Interface, w BatchPointsWriter) *SinglePointWriter {
	return &SinglePointWriter{
		log:    log,
		writer: w,
	}
}

// Write creates new influxdb.BatchPoints containing p and delegates that to the writer
func (w *SinglePointWriter) Write(bpConf influxdb.BatchPointsConfig, p *influxdb.Point) error {
	bp := newBatchPoints(bpConf)
	bp.AddPoint(p)
	return w.writer.Write(bp)
}

// batchPoint is a respresenation of a point written by BatchingWriter.
// Result of writing the batch of points containing the wrapped *influxdb.Point must be reported on errch.
type batchPoint struct {
	*influxdb.Point
	errch chan error
}

// pushError reports err(can be nil) to the waiter.
// It does not block and must only be executed once.
func (p *batchPoint) pushError(err error) {
	p.errch <- err
	close(p.errch)
}

func writeInBatches(log ttnlog.Interface, w BatchPointsWriter, bpConf influxdb.BatchPointsConfig, scalingInterval time.Duration, ch <-chan *batchPoint, keepalive bool) {
	log = log.WithField("config", bpConf)
	log.Info("Batching writer instance created")

	var points []*batchPoint
	for {
		select {
		case p := <-ch:
			points = append(points, p)
		default:
			if len(points) == 0 {
				select {
				case p := <-ch:
					points = append(points, p)
					continue
				case <-time.After(scalingInterval):
					if !keepalive {
						log.Info("Removing batching writer instance")
						return
					}
					points = append(points, <-ch)
					continue
				}
			}

			bp := newBatchPoints(bpConf)
			for _, p := range points {
				bp.AddPoint(p.Point)
			}

			log.WithField("num", len(points)).Debug("Writing batch of points to Influx")
			err := w.Write(bp)
			for _, p := range points {
				go p.pushError(err)
			}
			points = points[:0]
		}
	}
}

// BatchingWriter is a PointWriter, which writes points in batches.
// BatchingWriter scales automatically once it notices a delay of scalingInterval to write a batch of points and downscales if no points are supplied to an instance for a duration of scalingInterval.
// BatchingWriter spawns an instance for each unique BatchPointsConfig specified and up to limit additional instances on top of that.
// BatchingWriter does not limit the amount of instances if limit is nil.
// Each instance is spawned in a separate goroutine.
type BatchingWriter struct {
	log             ttnlog.Interface
	writer          BatchPointsWriter
	scalingInterval time.Duration

	activeMutex sync.RWMutex
	active      uint
	limitMutex  sync.RWMutex
	limit       uint

	pointChanMutex sync.RWMutex
	pointChans     map[influxdb.BatchPointsConfig]chan *batchPoint

	spawn func(log ttnlog.Interface, bpConf influxdb.BatchPointsConfig, ch chan *batchPoint)
}

// NewBatchingWriter creates new BatchingWriter. If limit == nil, then BatchingWriter does not limit amount of instances spawned.
func NewBatchingWriter(log ttnlog.Interface, w BatchPointsWriter, scalingInterval time.Duration, limit *uint) *BatchingWriter {
	bw := &BatchingWriter{
		log:             log,
		writer:          w,
		scalingInterval: scalingInterval,
		pointChans:      make(map[influxdb.BatchPointsConfig]chan *batchPoint),
	}
	if limit != nil {
		bw.limit = *limit
		bw.log = bw.log.WithField("limit", bw.limit)
		bw.spawn = bw.spawnInstanceWithCheck
	} else {
		bw.spawn = bw.spawnInstanceWithoutCheck
	}
	return bw
}

func (w *BatchingWriter) spawnInstanceWithoutCheck(log ttnlog.Interface, bpConf influxdb.BatchPointsConfig, ch chan *batchPoint) {
	w.activeMutex.Lock()
	w.active++
	go writeInBatches(w.log, w.writer, bpConf, w.scalingInterval, ch, false)
	w.activeMutex.Unlock()
}

func (w *BatchingWriter) spawnInstanceWithCheck(log ttnlog.Interface, bpConf influxdb.BatchPointsConfig, ch chan *batchPoint) {
	w.limitMutex.RLock()
	w.activeMutex.RLock()
	spawnNew := w.active < w.limit
	w.activeMutex.RUnlock()

	if spawnNew {
		w.activeMutex.Lock()
		if w.active < w.limit {
			w.active++
			go writeInBatches(w.log, w.writer, bpConf, w.scalingInterval, ch, false)
		}
		w.activeMutex.Unlock()
	}
	w.limitMutex.RUnlock()
}

// Write delegates p to a running instance of BatchingWriter and spawns new instances as required.
func (w *BatchingWriter) Write(bpConf influxdb.BatchPointsConfig, p *influxdb.Point) error {
	w.pointChanMutex.RLock()
	ch, ok := w.pointChans[bpConf]
	w.pointChanMutex.RUnlock()
	if !ok {
		w.pointChanMutex.Lock()
		ch, ok = w.pointChans[bpConf]
		if !ok {
			w.activeMutex.Lock()
			w.active++
			w.activeMutex.Unlock()

			if w.limit != 0 {
				w.limitMutex.Lock()
				w.limit++
				w.limitMutex.Unlock()
			}

			ch = make(chan *batchPoint)
			w.pointChans[bpConf] = ch
			go writeInBatches(w.log, w.writer, bpConf, w.scalingInterval, ch, true)
		}
		w.pointChanMutex.Unlock()
	}

	point := &batchPoint{
		Point: p,
		errch: make(chan error, 1),
	}
	select {
	case ch <- point:
	case <-time.After(w.scalingInterval):
		w.spawn(w.log, bpConf, ch)
		ch <- point
	}
	return <-point.errch
}