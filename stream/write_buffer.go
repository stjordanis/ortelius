// (c) 2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package stream

import (
	"context"
	"time"

	"github.com/ava-labs/ortelius/services/metrics"

	"github.com/ava-labs/avalanchego/utils/logging"

	"github.com/ava-labs/avalanchego/utils/hashing"
	"github.com/segmentio/kafka-go"
)

const (
	defaultBufferedWriterSize         = 256
	defaultBufferedWriterMsgQueueSize = defaultBufferedWriterSize * 5
	defaultWriteTimeout               = 1 * time.Minute
	defaultWriteRetry                 = 10
	defaultWriteRetrySleep            = 1 * time.Second
)

var defaultBufferedWriterFlushInterval = 1 * time.Second

// bufferedWriter takes in messages and writes them in batches to the backend.
type bufferedWriter struct {
	writer *kafka.Writer
	buffer chan (*[]byte)
	doneCh chan (struct{})
	log    logging.Logger

	// metrics
	metricSuccessCountKey       string
	metricFailureCountKey       string
	metricProcessMillisCountKey string
	flushTicker                 *time.Ticker
}

func newBufferedWriter(log logging.Logger, brokers []string, topic string) *bufferedWriter {
	size := defaultBufferedWriterSize

	wb := &bufferedWriter{
		writer: kafka.NewWriter(kafka.WriterConfig{
			Brokers:      brokers,
			Topic:        topic,
			Balancer:     &kafka.LeastBytes{},
			BatchBytes:   ConsumerMaxBytesDefault,
			BatchSize:    defaultBufferedWriterSize,
			WriteTimeout: defaultWriteTimeout,
			RequiredAcks: int(kafka.RequireAll),
		}),
		buffer:                      make(chan *[]byte, defaultBufferedWriterMsgQueueSize),
		doneCh:                      make(chan struct{}),
		log:                         log,
		metricSuccessCountKey:       "kafka_write_records_success",
		metricFailureCountKey:       "kafka_write_records_failure",
		metricProcessMillisCountKey: "kafka_write_records_process_millis",
	}

	metrics.Prometheus.CounterInit(wb.metricSuccessCountKey, "records success")
	metrics.Prometheus.CounterInit(wb.metricFailureCountKey, "records failure")
	metrics.Prometheus.CounterInit(wb.metricProcessMillisCountKey, "records processed millis")

	wb.flushTicker = time.NewTicker(defaultBufferedWriterFlushInterval)
	go wb.loop(size, defaultBufferedWriterFlushInterval)

	return wb
}

// Write adds the message to the buffer.
func (wb *bufferedWriter) Write(msg []byte) {
	wb.buffer <- &msg
}

// loop takes in messages from the buffer and commits them to Kafka when in
// batches
func (wb *bufferedWriter) loop(size int, flushInterval time.Duration) {
	var (
		lastFlush = time.Now()

		bufferSize = 0
		buffer     = make([](*[]byte), size)
		buffer2    = make([]kafka.Message, size)
	)

	flush := func() {
		defer func() { lastFlush = time.Now() }()

		if bufferSize == 0 {
			return
		}

		for bpos, b := range buffer[:bufferSize] {
			// reset the message..
			buffer2[bpos] = kafka.Message{}
			buffer2[bpos].Value = *b
			// compute hash before processing.
			buffer2[bpos].Key = hashing.ComputeHash256(buffer2[bpos].Value)
		}

		collectors := metrics.NewCollectors(
			metrics.NewCounterObserveMillisCollect(wb.metricProcessMillisCountKey),
			metrics.NewSuccessFailCounterAdd(wb.metricSuccessCountKey, wb.metricFailureCountKey, float64(bufferSize)),
		)
		defer func() {
			err := collectors.Collect()
			if err != nil {
				wb.log.Error("collectors.Collect: %s", err)
			}
		}()

		wm := func(bufmsg []kafka.Message, bufmsgsz int) error {
			ctx, cancelFn := context.WithTimeout(context.Background(), defaultWriteTimeout)
			defer cancelFn()

			return wb.writer.WriteMessages(ctx, bufmsg[:bufmsgsz]...)
		}

		var err error
		for icnt := 0; icnt < defaultWriteRetry; icnt++ {
			err = wm(buffer2, bufferSize)
			if err == nil {
				break
			}
			wb.log.Warn("Error writing to kafka (retry):", err)
			time.Sleep(defaultWriteRetrySleep)
		}

		if err != nil {
			collectors.Error()
			wb.log.Error("Error writing to kafka:", err)
		}

		bufferSize = 0
	}

	defer func() {
		wb.flushTicker.Stop()
		flush()
		close(wb.doneCh)
	}()

	for {
		select {
		case msg, ok := <-wb.buffer:
			if !ok {
				return
			}

			// If the buffer is full we must flush before we can add another message
			// This will exert backpressure
			if bufferSize >= size {
				flush()
			}

			// Add this message to the buffer and if it's full we flush and
			buffer[bufferSize] = msg
			bufferSize++
		case <-wb.flushTicker.C:
			// Don't flush if we've flushed recently from a full buffer
			if time.Now().After(lastFlush.Add(flushInterval)) {
				flush()
			}
		}
	}
}

// close stops the bufferedWriter and flushes any remaining items
func (wb *bufferedWriter) close() error {
	// Close buffer and wait for it to stop, flush, and signal back
	close(wb.buffer)
	wb.flushTicker.Stop()
	<-wb.doneCh
	return wb.writer.Close()
}
