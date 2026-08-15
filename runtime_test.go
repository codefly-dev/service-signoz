package main

import (
	"context"
	"os"
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

	config, err := os.ReadFile("clickhouse/config.d/signoz-cluster.xml")
	require.NoError(t, err)
	require.Contains(t, string(config), "<keeper_server>")
	require.Contains(t, string(config), "<cluster>")
	require.Contains(t, string(config), "from_env=\"CLICKHOUSE_PASSWORD\"")
}
