// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package otelarrowreceiver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	arrowpb "github.com/open-telemetry/otel-arrow/api/experimental/arrow/v1"
	"github.com/open-telemetry/otel-arrow/collector/testdata"
	"github.com/open-telemetry/otel-arrow/collector/testutil"
	arrowRecord "github.com/open-telemetry/otel-arrow/pkg/otel/arrow_record"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/client"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/config/configauth"
	"go.opentelemetry.io/collector/config/configgrpc"
	"go.opentelemetry.io/collector/config/confignet"
	"go.opentelemetry.io/collector/config/configtelemetry"
	"go.opentelemetry.io/collector/config/configtls"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/extension/auth"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/receivertest"
	"go.uber.org/mock/gomock"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/open-telemetry/otel-arrow/collector/receiver/otelarrowreceiver/internal/arrow/mock"
	componentMetadata "github.com/open-telemetry/otel-arrow/collector/receiver/otelarrowreceiver/internal/metadata"
)

const otlpReceiverName = "receiver_test"

var testReceiverID = component.NewIDWithName(componentMetadata.Type, otlpReceiverName)

func TestGRPCNewPortAlreadyUsed(t *testing.T) {
	addr := testutil.GetAvailableLocalAddress(t)
	ln, err := net.Listen("tcp", addr)
	require.NoError(t, err, "failed to listen on %q: %v", addr, err)
	t.Cleanup(func() {
		assert.NoError(t, ln.Close())
	})
	tt := componenttest.NewNopTelemetrySettings()
	r := newGRPCReceiver(t, addr, tt, consumertest.NewNop(), consumertest.NewNop())
	require.NotNil(t, r)

	require.Error(t, r.Start(context.Background(), componenttest.NewNopHost()))
}

// TestOTelArrowReceiverGRPCTracesIngestTest checks that the gRPC trace receiver
// is returning the proper response (return and metrics) when the next consumer
// in the pipeline reports error. The test changes the responses returned by the
// next trace consumer, checks if data was passed down the pipeline and if
// proper metrics were recorded. It also uses all endpoints supported by the
// trace receiver.
func TestOTelArrowReceiverGRPCTracesIngestTest(t *testing.T) {
	type ingestionStateTest struct {
		okToIngest   bool
		expectedCode codes.Code
	}

	expectedReceivedBatches := 2
	ingestionStates := []ingestionStateTest{
		{
			okToIngest:   true,
			expectedCode: codes.OK,
		},
		{
			okToIngest:   false,
			expectedCode: codes.Unknown,
		},
		{
			okToIngest:   true,
			expectedCode: codes.OK,
		},
	}

	addr := testutil.GetAvailableLocalAddress(t)
	td := testdata.GenerateTraces(1)

	tt, err := componenttest.SetupTelemetry(testReceiverID)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tt.Shutdown(context.Background())) })

	sink := &errOrSinkConsumer{TracesSink: new(consumertest.TracesSink)}

	ocr := newGRPCReceiver(t, addr, tt.TelemetrySettings(), sink, nil)
	require.NotNil(t, ocr)
	require.NoError(t, ocr.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, ocr.Shutdown(context.Background())) })

	cc, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	require.NoError(t, err)
	defer func() {
		assert.NoError(t, cc.Close())
	}()

	for _, ingestionState := range ingestionStates {
		if ingestionState.okToIngest {
			sink.SetConsumeError(nil)
		} else {
			sink.SetConsumeError(errors.New("consumer error"))
		}

		_, err = ptraceotlp.NewGRPCClient(cc).Export(context.Background(), ptraceotlp.NewExportRequestFromTraces(td))
		errStatus, ok := status.FromError(err)
		require.True(t, ok)
		assert.Equal(t, ingestionState.expectedCode, errStatus.Code())
	}

	require.Equal(t, expectedReceivedBatches, len(sink.AllTraces()))

	expectedIngestionBlockedRPCs := 1
	require.NoError(t, tt.CheckReceiverTraces("grpc", int64(expectedReceivedBatches), int64(expectedIngestionBlockedRPCs)))
}

func TestGRPCInvalidTLSCredentials(t *testing.T) {
	cfg := &Config{
		Protocols: Protocols{
			GRPC: configgrpc.ServerConfig{
				NetAddr: confignet.AddrConfig{
					Endpoint:  testutil.GetAvailableLocalAddress(t),
					Transport: confignet.TransportTypeTCP,
				},
				TLSSetting: &configtls.ServerConfig{
					Config: configtls.Config{
						CertFile: "willfail",
					},
				},
			},
		},
	}

	r, err := NewFactory().CreateTracesReceiver(
		context.Background(),
		receivertest.NewNopCreateSettings(),
		cfg,
		consumertest.NewNop())

	require.NoError(t, err)
	assert.NotNil(t, r)

	assert.EqualError(t,
		r.Start(context.Background(), componenttest.NewNopHost()),
		`failed to load TLS config: failed to load TLS cert and key: for auth via TLS, provide both certificate and key, or neither`)
}

func TestGRPCMaxRecvSize(t *testing.T) {
	addr := testutil.GetAvailableLocalAddress(t)
	sink := new(consumertest.TracesSink)

	factory := NewFactory()
	cfg := factory.CreateDefaultConfig().(*Config)
	cfg.GRPC.NetAddr.Endpoint = addr
	tt := componenttest.NewNopTelemetrySettings()
	ocr := newReceiver(t, factory, tt, cfg, testReceiverID, sink, nil)

	require.NotNil(t, ocr)
	require.NoError(t, ocr.Start(context.Background(), componenttest.NewNopHost()))

	cc, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	require.NoError(t, err)

	td := testdata.GenerateTraces(50000)
	require.Error(t, exportTraces(cc, td))
	assert.NoError(t, cc.Close())
	require.NoError(t, ocr.Shutdown(context.Background()))

	cfg.GRPC.MaxRecvMsgSizeMiB = 100

	ocr = newReceiver(t, factory, tt, cfg, testReceiverID, sink, nil)

	require.NotNil(t, ocr)
	require.NoError(t, ocr.Start(context.Background(), componenttest.NewNopHost()))
	t.Cleanup(func() { require.NoError(t, ocr.Shutdown(context.Background())) })

	cc, err = grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	require.NoError(t, err)
	defer func() {
		assert.NoError(t, cc.Close())
	}()

	td = testdata.GenerateTraces(50000)
	require.NoError(t, exportTraces(cc, td))
	require.Len(t, sink.AllTraces(), 1)
	assert.Equal(t, td, sink.AllTraces()[0])
}

func newGRPCReceiver(t *testing.T, endpoint string, settings component.TelemetrySettings, tc consumer.Traces, mc consumer.Metrics) component.Component {
	factory := NewFactory()
	cfg := factory.CreateDefaultConfig().(*Config)
	cfg.GRPC.NetAddr.Endpoint = endpoint
	return newReceiver(t, factory, settings, cfg, testReceiverID, tc, mc)
}

func newReceiver(t *testing.T, factory receiver.Factory, settings component.TelemetrySettings, cfg *Config, id component.ID, tc consumer.Traces, mc consumer.Metrics) component.Component {
	set := receivertest.NewNopCreateSettings()
	set.TelemetrySettings = settings
	set.TelemetrySettings.MetricsLevel = configtelemetry.LevelNormal
	set.ID = id
	var r component.Component
	var err error
	if tc != nil {
		r, err = factory.CreateTracesReceiver(context.Background(), set, cfg, tc)
		require.NoError(t, err)
	}
	if mc != nil {
		r, err = factory.CreateMetricsReceiver(context.Background(), set, cfg, mc)
		require.NoError(t, err)
	}
	return r
}

type senderFunc func(td ptrace.Traces)

func TestShutdown(t *testing.T) {
	endpointGrpc := testutil.GetAvailableLocalAddress(t)

	nextSink := new(consumertest.TracesSink)

	// Create OTelArrow receiver
	factory := NewFactory()
	cfg := factory.CreateDefaultConfig().(*Config)
	cfg.GRPC.NetAddr.Endpoint = endpointGrpc
	set := receivertest.NewNopCreateSettings()
	set.ID = testReceiverID
	r, err := NewFactory().CreateTracesReceiver(
		context.Background(),
		set,
		cfg,
		nextSink)
	require.NoError(t, err)
	require.NotNil(t, r)
	require.NoError(t, r.Start(context.Background(), componenttest.NewNopHost()))

	conn, err := grpc.Dial(endpointGrpc, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	require.NoError(t, err)
	defer conn.Close()

	doneSignalGrpc := make(chan bool)

	senderGrpc := func(td ptrace.Traces) {
		// Ignore error, may be executed after the receiver shutdown.
		_ = exportTraces(conn, td)
	}

	// Send traces to the receiver until we signal via done channel, and then
	// send one more trace after that.
	go generateTraces(senderGrpc, doneSignalGrpc)

	// Wait until the receiver outputs anything to the sink.
	assert.Eventually(t, func() bool {
		return nextSink.SpanCount() > 0
	}, time.Second, 10*time.Millisecond)

	// Now shutdown the receiver, while continuing sending traces to it.
	ctx, cancelFn := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelFn()
	err = r.Shutdown(ctx)
	assert.NoError(t, err)

	// Remember how many spans the sink received. This number should not change after this
	// point because after Shutdown() returns the component is not allowed to produce
	// any more data.
	sinkSpanCountAfterShutdown := nextSink.SpanCount()

	// Now signal to generateTraces to exit the main generation loop, then send
	// one more trace and stop.
	doneSignalGrpc <- true

	// Wait until all follow up traces are sent.
	<-doneSignalGrpc

	// The last, additional trace should not be received by sink, so the number of spans in
	// the sink should not change.
	assert.EqualValues(t, sinkSpanCountAfterShutdown, nextSink.SpanCount())
}

func generateTraces(senderFn senderFunc, doneSignal chan bool) {
	// Continuously generate spans until signaled to stop.
loop:
	for {
		select {
		case <-doneSignal:
			break loop
		default:
		}
		senderFn(testdata.GenerateTraces(1))
	}

	// After getting the signal to stop, send one more span and then
	// finally stop. We should never receive this last span.
	senderFn(testdata.GenerateTraces(1))

	// Indicate that we are done.
	close(doneSignal)
}

func exportTraces(cc *grpc.ClientConn, td ptrace.Traces) error {
	acc := ptraceotlp.NewGRPCClient(cc)
	req := ptraceotlp.NewExportRequestFromTraces(td)
	_, err := acc.Export(context.Background(), req)

	return err
}

type errOrSinkConsumer struct {
	*consumertest.TracesSink
	*consumertest.MetricsSink
	mu           sync.Mutex
	consumeError error // to be returned by ConsumeTraces, if set
}

// SetConsumeError sets an error that will be returned by the Consume function.
func (esc *errOrSinkConsumer) SetConsumeError(err error) {
	esc.mu.Lock()
	defer esc.mu.Unlock()
	esc.consumeError = err
}

func (esc *errOrSinkConsumer) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

// ConsumeTraces stores traces to this sink.
func (esc *errOrSinkConsumer) ConsumeTraces(ctx context.Context, td ptrace.Traces) error {
	esc.mu.Lock()
	defer esc.mu.Unlock()

	if esc.consumeError != nil {
		return esc.consumeError
	}

	return esc.TracesSink.ConsumeTraces(ctx, td)
}

// ConsumeMetrics stores metrics to this sink.
func (esc *errOrSinkConsumer) ConsumeMetrics(ctx context.Context, md pmetric.Metrics) error {
	esc.mu.Lock()
	defer esc.mu.Unlock()

	if esc.consumeError != nil {
		return esc.consumeError
	}

	return esc.MetricsSink.ConsumeMetrics(ctx, md)
}

// Reset deletes any stored in the sinks, resets error to nil.
func (esc *errOrSinkConsumer) Reset() {
	esc.mu.Lock()
	defer esc.mu.Unlock()

	esc.consumeError = nil
	if esc.TracesSink != nil {
		esc.TracesSink.Reset()
	}
	if esc.MetricsSink != nil {
		esc.MetricsSink.Reset()
	}
}

type tracesSinkWithMetadata struct {
	consumertest.TracesSink

	lock sync.Mutex
	mds  []client.Metadata
}

func (ts *tracesSinkWithMetadata) ConsumeTraces(ctx context.Context, td ptrace.Traces) error {
	info := client.FromContext(ctx)
	ts.lock.Lock()
	defer ts.lock.Unlock()
	ts.mds = append(ts.mds, info.Metadata)
	return ts.TracesSink.ConsumeTraces(ctx, td)
}

func (ts *tracesSinkWithMetadata) Metadatas() []client.Metadata {
	ts.lock.Lock()
	defer ts.lock.Unlock()
	return ts.mds
}

type anyStreamClient interface {
	Send(*arrowpb.BatchArrowRecords) error
	Recv() (*arrowpb.BatchStatus, error)
	grpc.ClientStream
}

func TestGRPCArrowReceiver(t *testing.T) {
	addr := testutil.GetAvailableLocalAddress(t)
	sink := new(tracesSinkWithMetadata)

	factory := NewFactory()
	cfg := factory.CreateDefaultConfig().(*Config)
	cfg.GRPC.NetAddr.Endpoint = addr
	cfg.GRPC.IncludeMetadata = true
	id := component.NewID(component.MustNewType("arrow"))
	tt := componenttest.NewNopTelemetrySettings()
	ocr := newReceiver(t, factory, tt, cfg, id, sink, nil)

	require.NotNil(t, ocr)
	require.NoError(t, ocr.Start(context.Background(), componenttest.NewNopHost()))

	cc, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stream anyStreamClient
	client := arrowpb.NewArrowTracesServiceClient(cc)
	stream, err = client.ArrowTraces(ctx, grpc.WaitForReady(true))
	require.NoError(t, err)
	producer := arrowRecord.NewProducer()

	var headerBuf bytes.Buffer
	hpd := hpack.NewEncoder(&headerBuf)

	var expectTraces []ptrace.Traces
	var expectMDs []metadata.MD

	// Repeatedly send traces via arrow. Set the expected traces
	// metadata to receive.
	for i := 0; i < 10; i++ {
		td := testdata.GenerateTraces(2)
		expectTraces = append(expectTraces, td)

		headerBuf.Reset()
		err := hpd.WriteField(hpack.HeaderField{
			Name:  "seq",
			Value: fmt.Sprint(i),
		})
		require.NoError(t, err)
		err = hpd.WriteField(hpack.HeaderField{
			Name:  "test",
			Value: "value",
		})
		require.NoError(t, err)
		expectMDs = append(expectMDs, metadata.MD{
			"seq":  []string{fmt.Sprint(i)},
			"test": []string{"value"},
		})

		batch, err := producer.BatchArrowRecordsFromTraces(td)
		require.NoError(t, err)

		batch.Headers = headerBuf.Bytes()

		err = stream.Send(batch)

		require.NoError(t, err)

		resp, err := stream.Recv()
		require.NoError(t, err)
		require.Equal(t, batch.BatchId, resp.BatchId)
		require.Equal(t, arrowpb.StatusCode_OK, resp.StatusCode)
	}

	assert.NoError(t, cc.Close())
	require.NoError(t, ocr.Shutdown(context.Background()))

	assert.Equal(t, expectTraces, sink.AllTraces())

	assert.Equal(t, len(expectMDs), len(sink.Metadatas()))
	// gRPC adds its own metadata keys, so we check for only the
	// expected ones below:
	for idx := range expectMDs {
		for key, vals := range expectMDs[idx] {
			require.Equal(t, vals, sink.Metadatas()[idx].Get(key), "for key %s", key)
		}
	}
}

type hostWithExtensions struct {
	component.Host
	exts map[component.ID]component.Component
}

func newHostWithExtensions(exts map[component.ID]component.Component) component.Host {
	return &hostWithExtensions{
		Host: componenttest.NewNopHost(),
		exts: exts,
	}
}

func (h *hostWithExtensions) GetExtensions() map[component.ID]component.Component {
	return h.exts
}

func newTestAuthExtension(t *testing.T, authFunc func(ctx context.Context, hdrs map[string][]string) (context.Context, error)) auth.Server {
	ctrl := gomock.NewController(t)
	as := mock.NewMockServer(ctrl)
	as.EXPECT().Authenticate(gomock.Any(), gomock.Any()).AnyTimes().DoAndReturn(authFunc)
	return as
}

func TestGRPCArrowReceiverAuth(t *testing.T) {
	addr := testutil.GetAvailableLocalAddress(t)
	sink := new(tracesSinkWithMetadata)

	authID := component.NewID(component.MustNewType("testauth"))

	factory := NewFactory()
	cfg := factory.CreateDefaultConfig().(*Config)
	cfg.GRPC.NetAddr.Endpoint = addr
	cfg.GRPC.IncludeMetadata = true
	cfg.GRPC.Auth = &configauth.Authentication{
		AuthenticatorID: authID,
	}
	id := component.NewID(component.MustNewType("arrow"))
	tt := componenttest.NewNopTelemetrySettings()
	ocr := newReceiver(t, factory, tt, cfg, id, sink, nil)

	require.NotNil(t, ocr)

	const errorString = "very much not authorized"

	type inStreamCtx struct{}

	host := newHostWithExtensions(
		map[component.ID]component.Component{
			authID: newTestAuthExtension(t, func(ctx context.Context, _ map[string][]string) (context.Context, error) {
				if ctx.Value(inStreamCtx{}) != nil {
					return ctx, fmt.Errorf(errorString)
				}
				return context.WithValue(ctx, inStreamCtx{}, t), nil
			}),
		},
	)

	require.NoError(t, ocr.Start(context.Background(), host))

	cc, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := arrowpb.NewArrowTracesServiceClient(cc)
	stream, err := client.ArrowTraces(ctx, grpc.WaitForReady(true))
	require.NoError(t, err)
	producer := arrowRecord.NewProducer()

	// Repeatedly send traces via arrow. Expect an auth error.
	for i := 0; i < 10; i++ {
		td := testdata.GenerateTraces(2)

		batch, err := producer.BatchArrowRecordsFromTraces(td)
		require.NoError(t, err)

		err = stream.Send(batch)
		require.NoError(t, err)

		resp, err := stream.Recv()
		require.NoError(t, err)
		// The stream has to be successful to get this far.  The
		// authenticator fails every data item:
		require.Equal(t, batch.BatchId, resp.BatchId)
		require.Equal(t, arrowpb.StatusCode_UNAUTHENTICATED, resp.StatusCode)
		require.Contains(t, resp.StatusMessage, errorString)
	}

	assert.NoError(t, cc.Close())
	require.NoError(t, ocr.Shutdown(context.Background()))

	assert.Equal(t, 0, len(sink.AllTraces()))
}

func TestConcurrentArrowReceiver(t *testing.T) {
	addr := testutil.GetAvailableLocalAddress(t)
	sink := new(tracesSinkWithMetadata)

	factory := NewFactory()
	cfg := factory.CreateDefaultConfig().(*Config)
	cfg.GRPC.NetAddr.Endpoint = addr
	cfg.GRPC.IncludeMetadata = true
	id := component.NewID(component.MustNewType("arrow"))
	tt := componenttest.NewNopTelemetrySettings()
	ocr := newReceiver(t, factory, tt, cfg, id, sink, nil)

	require.NotNil(t, ocr)
	require.NoError(t, ocr.Start(context.Background(), componenttest.NewNopHost()))

	cc, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const itemsPerStream = 10
	const numStreams = 5

	var wg sync.WaitGroup
	wg.Add(numStreams)

	for j := 0; j < numStreams; j++ {
		go func() {
			defer wg.Done()

			client := arrowpb.NewArrowTracesServiceClient(cc)
			stream, err := client.ArrowTraces(ctx, grpc.WaitForReady(true))
			require.NoError(t, err)
			producer := arrowRecord.NewProducer()

			var headerBuf bytes.Buffer
			hpd := hpack.NewEncoder(&headerBuf)

			// Repeatedly send traces via arrow. Set the expected traces
			// metadata to receive.
			for i := 0; i < itemsPerStream; i++ {
				td := testdata.GenerateTraces(2)

				headerBuf.Reset()
				err := hpd.WriteField(hpack.HeaderField{
					Name:  "seq",
					Value: fmt.Sprint(i),
				})
				require.NoError(t, err)

				batch, err := producer.BatchArrowRecordsFromTraces(td)
				require.NoError(t, err)

				batch.Headers = headerBuf.Bytes()

				err = stream.Send(batch)

				require.NoError(t, err)

				resp, err := stream.Recv()
				require.NoError(t, err)
				require.Equal(t, batch.BatchId, resp.BatchId)
				require.Equal(t, arrowpb.StatusCode_OK, resp.StatusCode)
			}
		}()
	}
	wg.Wait()

	assert.NoError(t, cc.Close())
	require.NoError(t, ocr.Shutdown(context.Background()))

	counts := make([]int, itemsPerStream)

	// Two spans per stream/item.
	require.Equal(t, itemsPerStream*numStreams*2, sink.SpanCount())
	require.Equal(t, itemsPerStream*numStreams, len(sink.Metadatas()))

	for _, md := range sink.Metadatas() {
		val, err := strconv.Atoi(md.Get("seq")[0])
		require.NoError(t, err)
		counts[val]++
	}

	for i := 0; i < itemsPerStream; i++ {
		require.Equal(t, numStreams, counts[i])
	}
}
