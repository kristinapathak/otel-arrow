// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package arrow // import "github.com/open-telemetry/otel-arrow/collector/exporter/otelarrowexporter/internal/arrow"

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	arrowpb "github.com/open-telemetry/otel-arrow/api/experimental/arrow/v1"
	"github.com/open-telemetry/otel-arrow/collector/netstats"
	arrowRecord "github.com/open-telemetry/otel-arrow/pkg/otel/arrow_record"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configtelemetry"
	"go.opentelemetry.io/collector/consumer/consumererror"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/otel"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/multierr"
	"go.uber.org/zap"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// Stream is 1:1 with gRPC stream.
type Stream struct {
	// maxStreamLifetime is the max timeout before stream
	// should be closed on the client side. This ensures a
	// graceful shutdown before max_connection_age is reached
	// on the server side.
	maxStreamLifetime time.Duration

	// producer is exclusive to the holder of the stream.
	producer arrowRecord.ProducerAPI

	// prioritizer has a reference to the stream, this allows it to be severed.
	prioritizer *streamPrioritizer

	// perRPCCredentials from the auth extension, or nil.
	perRPCCredentials credentials.PerRPCCredentials

	// telemetry are a copy of the exporter's telemetry settings
	telemetry component.TelemetrySettings

	// tracer is used to create a span describing the export.
	tracer trace.Tracer

	// client uses the exporter's grpc.ClientConn.  this is
	// initially nil only set when ArrowStream() calls meaning the
	// endpoint recognizes OTel-Arrow.
	client AnyStreamClient

	// method the gRPC method name, used for additional instrumentation.
	method string

	// toWrite is passes a batch from the sender to the stream writer, which
	// includes a dedicated channel for the response.
	toWrite chan writeItem

	// lock protects waiters.
	lock sync.Mutex

	// waiters is the response channel for each active batch.
	waiters map[int64]chan error

	// netReporter provides network-level metrics.
	netReporter netstats.Interface
}

// writeItem is passed from the sender (a pipeline consumer) to the
// stream writer, which is not bound by the sender's context.
type writeItem struct {
	// records is a ptrace.Traces, plog.Logs, or pmetric.Metrics
	records any
	// md is the caller's metadata, derived from its context.
	md map[string]string
	// errCh is used by the stream reader to unblock the sender
	errCh chan error
	// uncompSize is computed by the appropriate sizer (in the
	// caller's goroutine)
	uncompSize int
	// parent will be used to create a span around the stream request.
	parent context.Context
}

// newStream constructs a stream
func newStream(
	producer arrowRecord.ProducerAPI,
	prioritizer *streamPrioritizer,
	telemetry component.TelemetrySettings,
	perRPCCredentials credentials.PerRPCCredentials,
	netReporter netstats.Interface,
) *Stream {
	tracer := telemetry.TracerProvider.Tracer("otel-arrow-exporter")
	return &Stream{
		producer:          producer,
		prioritizer:       prioritizer,
		perRPCCredentials: perRPCCredentials,
		telemetry:         telemetry,
		tracer:            tracer,
		toWrite:           make(chan writeItem, 1),
		waiters:           map[int64]chan error{},
		netReporter:       netReporter,
	}
}

// setBatchChannel places a waiting consumer's batchID into the waiters map, where
// the stream reader may find it.
func (s *Stream) setBatchChannel(batchID int64, errCh chan error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.waiters[batchID] = errCh
}

// logStreamError decides how to log an error.  `which` indicates the
// stream direction, will be "reader" or "writer".
func (s *Stream) logStreamError(which string, err error) {
	var code codes.Code
	var msg string
	// gRPC tends to supply status-wrapped errors, so we always
	// unpack them.  A wrapped Canceled code indicates intentional
	// shutdown, which can be due to normal causes (EOF, e.g.,
	// max-stream-lifetime reached) or unusual causes (Canceled,
	// e.g., because the other stream direction reached an error).
	if status, ok := status.FromError(err); ok {
		code = status.Code()
		msg = status.Message()
	} else if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		code = codes.Canceled
		msg = err.Error()
	} else {
		code = codes.Internal
		msg = err.Error()
	}
	if code == codes.Canceled {
		s.telemetry.Logger.Debug("arrow stream shutdown", zap.String("which", which), zap.String("message", msg))
	} else {
		s.telemetry.Logger.Error("arrow stream error", zap.String("which", which), zap.String("message", msg), zap.Int("code", int(code)))
	}
}

// run blocks the calling goroutine while executing stream logic.  run
// will return when the reader and writer are finished.  errors will be logged.
func (s *Stream) run(bgctx context.Context, streamClient StreamClientFunc, grpcOptions []grpc.CallOption) {
	ctx, cancel := context.WithCancel(bgctx)
	defer cancel()

	sc, method, err := streamClient(ctx, grpcOptions...)
	if err != nil {
		// Returning with stream.client == nil signals the
		// lack of an Arrow stream endpoint.  When all the
		// streams return with .client == nil, the ready
		// channel will be closed, which causes downgrade.
		//
		// Note: These are gRPC server internal errors and
		// will cause downgrade to standard OTLP.  These
		// cannot be simulated by connecting to a gRPC server
		// that does not support the ArrowStream service, with
		// or without the WaitForReady flag set.  In a real
		// gRPC server the first Unimplemented code is
		// generally delivered to the Recv() call below, so
		// this code path is not taken for an ordinary downgrade.
		s.telemetry.Logger.Error("cannot start arrow stream", zap.Error(err))
		return
	}
	// Setting .client != nil indicates that the endpoint was valid,
	// streaming may start.  When this stream finishes, it will be
	// restarted.
	s.method = method
	s.client = sc

	// ww is used to wait for the writer.  Since we wait for the writer,
	// the writer's goroutine is not added to exporter waitgroup (e.wg).
	var ww sync.WaitGroup

	var writeErr error
	ww.Add(1)
	go func() {
		defer ww.Done()
		writeErr = s.write(ctx)
		if writeErr != nil {
			cancel()
		}
	}()

	// the result from read() is processed after cancel and wait,
	// so we can set s.client = nil in case of a delayed Unimplemented.
	err = s.read(ctx)

	// Wait for the writer to ensure that all waiters are known.
	cancel()
	ww.Wait()

	if err != nil {
		// This branch is reached with an unimplemented status
		// with or without the WaitForReady flag.
		if status, ok := status.FromError(err); ok && status.Code() == codes.Unimplemented {
			// This (client == nil) signals the controller to
			// downgrade when all streams have returned in that
			// status.
			//
			// This is a special case because we reset s.client,
			// which sets up a downgrade after the streams return.
			s.client = nil
			s.telemetry.Logger.Info("arrow is not supported",
				zap.String("message", status.Message()),
			)
		} else {
			// All other cases, use the standard log handler.
			s.logStreamError("reader", err)
		}
	}
	if writeErr != nil {
		s.logStreamError("writer", writeErr)
	}

	// The reader and writer have both finished; respond to any
	// outstanding waiters.
	for _, ch := range s.waiters {
		// Note: the top-level OTLP exporter will retry.
		ch <- ErrStreamRestarting
	}
}

// write repeatedly places this stream into the next-available queue, then
// performs a blocking send().  This returns when the data is in the write buffer,
// the caller waiting on its error channel.
func (s *Stream) write(ctx context.Context) (retErr error) {
	// always close send()
	defer func() {
		s.client.CloseSend()
	}()

	// headers are encoding using hpack, reusing a buffer on each call.
	var hdrsBuf bytes.Buffer
	hdrsEnc := hpack.NewEncoder(&hdrsBuf)

	var timerCh <-chan time.Time
	if s.maxStreamLifetime != 0 {
		timer := time.NewTimer(s.maxStreamLifetime)
		timerCh = timer.C
		defer timer.Stop()
	}

	for {
		// Note: this can't block b/c stream has capacity &
		// individual streams shut down synchronously.
		s.prioritizer.setReady(s)

		// this can block, and if the context is canceled we
		// wait for the reader to find this stream.
		var wri writeItem
		var ok bool
		select {
		case <-timerCh:
			// If timerCh is nil, this will never happen.
			s.prioritizer.removeReady(s)
			return nil
		case wri, ok = <-s.toWrite:
			// channel is closed
			if !ok {
				return nil
			}
		case <-ctx.Done():
			// Because we did not <-stream.toWrite, there
			// is a potential sender race since the stream
			// is currently in the ready set.
			s.prioritizer.removeReady(s)
			return ctx.Err()
		}

		err := s.encodeAndSend(wri, &hdrsBuf, hdrsEnc)
		if err != nil {
			// Note: For the return statement below, there is no potential
			// sender race because the stream is not available, as indicated by
			// the successful <-stream.toWrite above
			return err
		}
	}
}

func (s *Stream) encodeAndSend(wri writeItem, hdrsBuf *bytes.Buffer, hdrsEnc *hpack.Encoder) (retErr error) {
	ctx, span := s.tracer.Start(wri.parent, "otel_arrow_stream_send")
	defer span.End()

	defer func() {
		// Set span status if an error is returned.
		if retErr != nil {
			span := trace.SpanFromContext(ctx)
			span.SetStatus(otelcodes.Error, retErr.Error())
		}
	}()
	// Get the global propagator, to inject context.  When there
	// are no fields, it's a no-op propagator implementation and
	// we can skip the allocations inside this block.
	prop := otel.GetTextMapPropagator()
	if len(prop.Fields()) > 0 {
		// When the incoming context carries nothing, the map
		// will be nil.  Allocate, if necessary.
		if wri.md == nil {
			wri.md = map[string]string{}
		}
		// Use the global propagator to inject trace context.  Note that
		// OpenTelemetry Collector will set a global propagator from the
		// service::telemetry::traces configuration.
		otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(wri.md))
	}

	batch, err := s.encode(wri.records)
	if err != nil {
		// This is some kind of internal error.  We will restart the
		// stream and mark this record as a permanent one.
		err = fmt.Errorf("encode: %w", err)
		wri.errCh <- consumererror.NewPermanent(err)
		return err
	}

	// Optionally include outgoing metadata, if present.
	if len(wri.md) != 0 {
		hdrsBuf.Reset()
		for key, val := range wri.md {
			err := hdrsEnc.WriteField(hpack.HeaderField{
				Name:  key,
				Value: val,
			})
			if err != nil {
				// This case is like the encode-failure case
				// above, we will restart the stream but consider
				// this a permenent error.
				err = fmt.Errorf("hpack: %w", err)
				wri.errCh <- consumererror.NewPermanent(err)
				return err
			}
		}
		batch.Headers = hdrsBuf.Bytes()
	}

	// Let the receiver knows what to look for.
	s.setBatchChannel(batch.BatchId, wri.errCh)

	// The netstats code knows that uncompressed size is
	// unreliable for arrow transport, so we instrument it
	// directly here.  Only the primary direction of transport
	// is instrumented this way.
	if wri.uncompSize != 0 {
		var sized netstats.SizesStruct
		sized.Method = s.method
		sized.Length = int64(wri.uncompSize)
		s.netReporter.CountSend(ctx, sized)
		s.netReporter.SetSpanSizeAttributes(ctx, sized)
	}

	if err := s.client.Send(batch); err != nil {
		// The error will be sent to errCh during cleanup for this stream.
		// Note: do not wrap this error, it may contain a Status.
		return err
	}

	return nil
}

// read repeatedly reads a batch status and releases the consumers waiting for
// a response.
func (s *Stream) read(_ context.Context) error {
	// Note we do not use the context, the stream context might
	// cancel a call to Recv() but the call to processBatchStatus
	// is non-blocking.
	for {
		// Note: if the client has called CloseSend() and is waiting for a response from the server.
		// And if the server fails for some reason, we will wait until some other condition, such as a context
		// timeout.  TODO: possibly, improve to wait for no outstanding requests and then stop reading.
		resp, err := s.client.Recv()
		if err != nil {
			// Note: do not wrap, contains a Status.
			return err
		}

		if err = s.processBatchStatus(resp); err != nil {
			return fmt.Errorf("process: %w", err)
		}
	}
}

// getSenderChannels takes the stream lock and removes the
// corresonding sender channel for each BatchId.  They are returned
// with the same index as the original status, for correlation.  Nil
// channels will be returned when there are errors locating the
// sender channel.
func (s *Stream) getSenderChannels(status *arrowpb.BatchStatus) (chan error, error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	ch, ok := s.waiters[status.BatchId]
	if !ok {
		// Will break the stream.
		return nil, fmt.Errorf("unrecognized batch ID: %d", status.BatchId)
	}
	delete(s.waiters, status.BatchId)
	return ch, nil
}

// processBatchStatus processes a single response from the server and unblocks the
// associated sender.
func (s *Stream) processBatchStatus(ss *arrowpb.BatchStatus) error {
	ch, ret := s.getSenderChannels(ss)

	if ch == nil {
		// In case getSenderChannels encounters a problem, the
		// channel is nil.
		return ret
	}

	if ss.StatusCode == arrowpb.StatusCode_OK {
		ch <- nil
		return nil
	}
	// See ../../otelarrow.go's `shouldRetry()` method, the retry
	// behavior described here is achieved there by setting these
	// recognized codes.
	var err error
	switch ss.StatusCode {
	case arrowpb.StatusCode_UNAVAILABLE:
		// Retryable
		err = status.Errorf(codes.Unavailable, "destination unavailable: %d: %s", ss.BatchId, ss.StatusMessage)
	case arrowpb.StatusCode_INVALID_ARGUMENT:
		// Not retryable
		err = status.Errorf(codes.InvalidArgument, "invalid argument: %d: %s", ss.BatchId, ss.StatusMessage)
	case arrowpb.StatusCode_RESOURCE_EXHAUSTED:
		// Retry behavior is configurable
		err = status.Errorf(codes.ResourceExhausted, "resource exhausted: %d: %s", ss.BatchId, ss.StatusMessage)
	default:
		// Note: a Canceled StatusCode was once returned by receivers following
		// a CloseSend() from the exporter.  This is now handled using error
		// status codes.  If an exporter is upgraded before a receiver, the exporter
		// will log this error when the receiver closes streams.

		// Unrecognized status code.
		err = status.Errorf(codes.Internal, "unexpected stream response: %d: %s", ss.BatchId, ss.StatusMessage)

		// Will break the stream.
		ret = multierr.Append(ret, err)
	}
	ch <- err
	return ret
}

// SendAndWait submits a batch of records to be encoded and sent.  Meanwhile, this
// goroutine waits on the incoming context or for the asynchronous response to be
// received by the stream reader.
func (s *Stream) SendAndWait(ctx context.Context, records any) error {
	errCh := make(chan error, 1)

	// Note that if the OTLP exporter's gRPC Headers field was
	// set, those (static) headers were used to establish the
	// stream.  The caller's context was returned by
	// baseExporter.enhanceContext() includes the static headers
	// plus optional client metadata.  Here, get whatever
	// headers that gRPC would have transmitted for a unary RPC
	// and convey them via the Arrow batch.

	// Note that the "uri" parameter to GetRequestMetadata is
	// not used by the headersetter extension and is not well
	// documented.  Since it's an optional list, we omit it.
	var md map[string]string
	if s.perRPCCredentials != nil {
		var err error
		md, err = s.perRPCCredentials.GetRequestMetadata(ctx)
		if err != nil {
			return err
		}
	}

	// Note that the uncompressed size as measured by the receiver
	// will be different than uncompressed size as measured by the
	// exporter, because of the optimization phase performed in the
	// conversion to Arrow.
	var uncompSize int
	if s.telemetry.MetricsLevel > configtelemetry.LevelNormal {
		switch data := records.(type) {
		case ptrace.Traces:
			var sizer ptrace.ProtoMarshaler
			uncompSize = sizer.TracesSize(data)
		case plog.Logs:
			var sizer plog.ProtoMarshaler
			uncompSize = sizer.LogsSize(data)
		case pmetric.Metrics:
			var sizer pmetric.ProtoMarshaler
			uncompSize = sizer.MetricsSize(data)
		}
	}

	s.toWrite <- writeItem{
		records:    records,
		md:         md,
		uncompSize: uncompSize,
		errCh:      errCh,
		parent:     ctx,
	}

	// Note this ensures the caller's timeout is respected.
	select {
	case <-ctx.Done():
		// This caller's context timed out.
		return ctx.Err()
	case err := <-errCh:
		// Note: includes err == nil and err != nil cases.
		return err
	}
}

// encode produces the next batch of Arrow records.
func (s *Stream) encode(records any) (_ *arrowpb.BatchArrowRecords, retErr error) {
	// Defensively, protect against panics in the Arrow producer function.
	defer func() {
		if err := recover(); err != nil {
			// When this happens, the stacktrace is
			// important and lost if we don't capture it
			// here.
			s.telemetry.Logger.Debug("panic detail in otel-arrow-adapter",
				zap.Reflect("recovered", err),
				zap.Stack("stacktrace"),
			)
			retErr = fmt.Errorf("panic in otel-arrow-adapter: %v", err)
		}
	}()
	var batch *arrowpb.BatchArrowRecords
	var err error
	switch data := records.(type) {
	case ptrace.Traces:
		batch, err = s.producer.BatchArrowRecordsFromTraces(data)
	case plog.Logs:
		batch, err = s.producer.BatchArrowRecordsFromLogs(data)
	case pmetric.Metrics:
		batch, err = s.producer.BatchArrowRecordsFromMetrics(data)
	default:
		return nil, fmt.Errorf("unsupported OTLP type: %T", records)
	}
	return batch, err
}
