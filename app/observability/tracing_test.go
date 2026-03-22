package observability

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestSetupTracing_NoopWhenEndpointEmpty(t *testing.T) {
	shutdown, err := SetupTracing(context.Background(), "test-service", "")
	require.NoError(t, err)
	require.NotNil(t, shutdown)

	tp := otel.GetTracerProvider()
	assert.IsType(t, noop.NewTracerProvider(), tp)

	require.NoError(t, shutdown(context.Background()))
}

func TestSetupTracing_WithEndpoint(t *testing.T) {
	shutdown, err := SetupTracing(context.Background(), "test-service", "http://localhost:4318")
	require.NoError(t, err)
	require.NotNil(t, shutdown)

	tp := otel.GetTracerProvider()
	assert.NotEqual(t, noop.NewTracerProvider(), tp)

	require.NoError(t, shutdown(context.Background()))
}

func TestSetupTracing_SetsPropagator(t *testing.T) {
	shutdown, err := SetupTracing(context.Background(), "test-service", "")
	require.NoError(t, err)
	defer func() { _ = shutdown(context.Background()) }()

	prop := otel.GetTextMapPropagator()
	assert.Contains(t, prop.Fields(), "traceparent")
}
