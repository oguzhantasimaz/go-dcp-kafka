package producer

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"syscall"
	"time"

	"github.com/Trendyol/go-dcp/logger"
	"github.com/Trendyol/go-dcp/models"
	"github.com/segmentio/kafka-go"
)

type Batch struct {
	logger              logger.Logger
	errorLogger         logger.Logger
	batchTicker         *time.Ticker
	Writer              *kafka.Writer
	dcpCheckpointCommit func()
	metric              *Metric
	messages            []kafka.Message
	currentMessageBytes int
	batchTickerDuration time.Duration
	batchLimit          int
	batchBytes          int
	flushLock           sync.Mutex
	isDcpRebalancing    bool
}

func newBatch(
	batchTime time.Duration,
	writer *kafka.Writer,
	batchLimit int,
	batchBytes int,
	logger logger.Logger,
	errorLogger logger.Logger,
	dcpCheckpointCommit func(),
) *Batch {
	batch := &Batch{
		batchTickerDuration: batchTime,
		batchTicker:         time.NewTicker(batchTime),
		metric:              &Metric{},
		messages:            make([]kafka.Message, 0, batchLimit),
		Writer:              writer,
		batchLimit:          batchLimit,
		logger:              logger,
		errorLogger:         errorLogger,
		dcpCheckpointCommit: dcpCheckpointCommit,
		batchBytes:          batchBytes,
	}
	return batch
}

func (b *Batch) StartBatchTicker() {
	go func() {
		for {
			<-b.batchTicker.C
			b.FlushMessages()
		}
	}()
}

func (b *Batch) Close() {
	b.batchTicker.Stop()
	b.FlushMessages()
}

func (b *Batch) PrepareStartRebalancing() {
	b.flushLock.Lock()
	defer b.flushLock.Unlock()

	b.isDcpRebalancing = true
	b.messages = b.messages[:0]
	b.currentMessageBytes = 0
}

func (b *Batch) PrepareEndRebalancing() {
	b.flushLock.Lock()
	defer b.flushLock.Unlock()

	b.isDcpRebalancing = false
}

func (b *Batch) AddMessages(ctx *models.ListenerContext, messages []kafka.Message, eventTime time.Time) {
	b.flushLock.Lock()
	if b.isDcpRebalancing {
		return
	}
	b.messages = append(b.messages, messages...)
	b.currentMessageBytes += binary.Size(messages)
	ctx.Ack()
	b.flushLock.Unlock()

	b.metric.KafkaConnectorLatency = time.Since(eventTime).Milliseconds()

	if len(b.messages) >= b.batchLimit || b.currentMessageBytes >= b.batchBytes {
		b.FlushMessages()
	}
}

func (b *Batch) FlushMessages() {
	b.flushLock.Lock()
	defer b.flushLock.Unlock()
	if len(b.messages) > 0 {
		startedTime := time.Now()
		err := b.Writer.WriteMessages(context.Background(), b.messages...)
		if err != nil {
			if isFatalError(err) {
				panic(fmt.Errorf("permanent error on Kafka side %e", err))
			}
			b.errorLogger.Printf("batch producer flush error %v", err)
			return
		}
		b.metric.BatchProduceLatency = time.Since(startedTime).Milliseconds()

		b.messages = b.messages[:0]
		b.currentMessageBytes = 0
		b.batchTicker.Reset(b.batchTickerDuration)
	}
	b.dcpCheckpointCommit()
}

func isFatalError(err error) bool {
	e, ok := err.(kafka.Error)

	if (ok && e.Temporary()) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) {
		return false
	}
	return true
}
