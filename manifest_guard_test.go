package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestManifestGuardRender(t *testing.T) {
	destination := os.Getenv("CODEFLY_MANIFEST_DESTINATION")
	if destination == "" {
		t.Skip("CODEFLY_MANIFEST_DESTINATION is unset; skipping manifest guard render")
	}
	environment := os.Getenv("CODEFLY_MANIFEST_ENVIRONMENT")
	namespace := os.Getenv("CODEFLY_MANIFEST_NAMESPACE")
	profileName := os.Getenv("CODEFLY_MANIFEST_PROFILE")
	require.NotEmpty(t, environment)
	require.NotEmpty(t, namespace)
	profile, ok := builderv0.KubernetesOutputProfile_value[profileName]
	require.Truef(t, ok, "unknown Kubernetes output profile %q", profileName)

	validation := renderManifests(t, destination, environment, namespace, builderv0.KubernetesOutputProfile(profile))
	require.Equal(t, builderv0.KubernetesManifestValidation_STATUS_PASSED, validation.GetStaticValidation(), validation.GetViolations())
	require.True(t, validation.GetRestricted())
}

func TestRestrictedRenderIsDeterministicAndPrivate(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	firstValidation := renderManifests(t, first, "test", "observability", builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1)
	secondValidation := renderManifests(t, second, "test", "observability", builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1)
	require.Equal(t, builderv0.KubernetesManifestValidation_STATUS_PASSED, firstValidation.GetStaticValidation(), firstValidation.GetViolations())
	require.Equal(t, builderv0.KubernetesManifestValidation_STATUS_PASSED, secondValidation.GetStaticValidation(), secondValidation.GetViolations())
	require.Equal(t, hashManifestTree(t, first), hashManifestTree(t, second))

	contents := manifestContents(t, first)
	require.NotContains(t, contents, "literal-password")
	require.NotContains(t, contents, "kind: Secret")
	require.NotContains(t, contents, "kind: Ingress")
	require.NotContains(t, contents, "type: LoadBalancer")
	require.NotContains(t, contents, "type: NodePort")
	require.Contains(t, contents, "secretKeyRef:")
	require.Contains(t, contents, "type: ClusterIP")
	require.Contains(t, contents, "kind: NetworkPolicy")

	for _, file := range []string{"query-stateful-set.yaml", "collector-deployment.yaml", "migrator-job.yaml"} {
		data, err := os.ReadFile(filepath.Join(first, "base", file))
		require.NoError(t, err)
		var document any
		require.NoError(t, yaml.Unmarshal(data, &document), file)
	}
}

func renderManifests(t *testing.T, destination, environment, namespace string, profile builderv0.KubernetesOutputProfile) *builderv0.KubernetesManifestValidation {
	t.Helper()
	ctx := context.Background()
	identity := &resources.ServiceIdentity{Workspace: "workspace", Module: "observability", Name: "signoz", Version: "0.0.0"}
	base := &services.Base{
		Wool:     wool.Get(ctx),
		Identity: identity,
		Information: &services.Information{
			Service: resources.ToServiceWithCase(identity),
			Module:  resources.ToModuleWithCase(identity),
		},
	}
	base.SetDockerImage(signozImage)
	builder := &services.BuilderWrapper{Base: base}
	base.Builder = builder
	references := map[string]*builderv0.KubernetesSecretKeyReference{
		"CLICKHOUSE_DSN":              {Name: "signoz-clickhouse", Key: "dsn"},
		"SIGNOZ_TOKENIZER_JWT_SECRET": {Name: "signoz-runtime", Key: "jwt-secret"},
	}
	deployment := &builderv0.KubernetesDeployment{
		Namespace: namespace, Destination: destination, Profile: profile, SecretReferences: references,
	}
	params := services.DeploymentParameters{
		SecretReferences: references,
		Parameters: DeploymentTemplateParameters{
			SigNozImage: signozImage.FullName(), CollectorImage: collectorImage.FullName(),
		},
	}
	require.NoError(t, builder.KustomizeDeploy(ctx, &basev0.Environment{Name: environment}, deployment, deploymentFS, params))
	return services.ValidateKubernetesManifestTree(ctx, destination, environment, namespace, profile, false, "", "")
}

func manifestContents(t *testing.T, root string) string {
	t.Helper()
	var contents strings.Builder
	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		contents.Write(data)
		return nil
	}))
	return contents.String()
}

func hashManifestTree(t *testing.T, root string) string {
	t.Helper()
	var paths []string
	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			paths = append(paths, path)
		}
		return err
	}))
	sort.Strings(paths)
	hash := sha256.New()
	for _, path := range paths {
		relative, err := filepath.Rel(root, path)
		require.NoError(t, err)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		_, _ = fmt.Fprintf(hash, "%s\x00%s\x00", relative, data)
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}
