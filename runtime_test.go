package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestParseClickHouseDependency(t *testing.T) {
	configuration := &basev0.Configuration{
		RuntimeContext: resources.NewRuntimeContextContainer(),
		Infos: []*basev0.ConfigurationInformation{{
			Name: "clickhouse",
			ConfigurationValues: []*basev0.ConfigurationValue{{
				Key: "connection", Value: "clickhouse://signoz:secret@host.docker.internal:39000/signoz", Secret: true,
			}},
		}},
	}
	connection, err := parseClickHouseDependency(context.Background(), []*basev0.Configuration{configuration})
	require.NoError(t, err)
	require.Equal(t, clickHouseConnection{Host: "host.docker.internal", Port: "39000", User: "signoz", Password: "secret"}, connection)
}

func TestParseClickHouseDependencyRejectsMissingCredentials(t *testing.T) {
	configuration := &basev0.Configuration{
		RuntimeContext: resources.NewRuntimeContextContainer(),
		Infos: []*basev0.ConfigurationInformation{{
			Name: "clickhouse",
			ConfigurationValues: []*basev0.ConfigurationValue{{
				Key: "connection", Value: "clickhouse://host.docker.internal:39000/signoz", Secret: true,
			}},
		}},
	}

	_, err := parseClickHouseDependency(context.Background(), []*basev0.Configuration{configuration})
	require.EqualError(t, err, "ClickHouse dependency connection is incomplete")
}

func TestCompanionClickHouseConfiguration(t *testing.T) {
	collectorConfig, err := runtimeFS.ReadFile("runtime/otel-collector-config.yaml")
	require.NoError(t, err)
	require.Contains(t, string(collectorConfig), "clickhousetraces:")
	require.Contains(t, string(collectorConfig), "signozspanmetrics/delta")
	require.Contains(t, string(collectorConfig), "${env:CLICKHOUSE_DSN}")
	require.NotContains(t, string(collectorConfig), "tcp://")

	config, err := os.ReadFile("clickhouse/config.d/signoz-cluster.xml")
	require.NoError(t, err)
	require.Contains(t, string(config), "<keeper_server>")
	require.Contains(t, string(config), "<cluster>")
	require.Contains(t, string(config), "from_env=\"CLICKHOUSE_PASSWORD\"")
}

func TestRuntimeShutdownRemovesCollectorConfiguration(t *testing.T) {
	runtime := NewRuntime()
	configDir := filepath.Join(t.TempDir(), "collector-config")
	runtime.configDir = configDir
	require.NoError(t, os.Mkdir(configDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("test"), 0o600))

	require.NoError(t, runtime.shutdown(context.Background()))
	_, err := os.Stat(configDir)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Empty(t, runtime.configDir)
	require.NoError(t, runtime.shutdown(context.Background()))
}
