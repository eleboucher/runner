package labels

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLabel_Parse_K8sPod(t *testing.T) {
	t.Run("k8spod with podspec path", func(t *testing.T) {
		label, err := Parse("myrunner:k8spod://config/podspec.yaml")
		require.NoError(t, err)
		assert.Equal(t, "myrunner", label.Name)
		assert.Equal(t, SchemeK8sPod, label.Schema)
		assert.Equal(t, "//config/podspec.yaml", label.Arg)
	})

	t.Run("k8spod without arg is an error", func(t *testing.T) {
		_, err := Parse("myrunner:k8spod")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "requires a podspec file path")
	})

	t.Run("k8spod with absolute path", func(t *testing.T) {
		label, err := Parse("gpu:k8spod:///etc/runner/podspec-gpu.yaml")
		require.NoError(t, err)
		assert.Equal(t, "gpu", label.Name)
		assert.Equal(t, SchemeK8sPod, label.Schema)
		assert.Equal(t, "///etc/runner/podspec-gpu.yaml", label.Arg)
	})
}

func TestLabel_String_K8sPod(t *testing.T) {
	label := &Label{Name: "myrunner", Schema: SchemeK8sPod, Arg: "//config/podspec.yaml"}
	assert.Equal(t, "myrunner:k8spod://config/podspec.yaml", label.String())
}

func TestLabels_PickPlatform_K8sPod(t *testing.T) {
	t.Run("matches k8spod label", func(t *testing.T) {
		labels := Labels{
			{Name: "ubuntu-latest", Schema: SchemeK8sPod, Arg: "//podspec.yaml"},
		}
		platform := labels.PickPlatform([]string{"ubuntu-latest"})
		assert.Equal(t, "k8spod", platform)
	})

	t.Run("falls back to docker default when no match", func(t *testing.T) {
		labels := Labels{
			{Name: "gpu", Schema: SchemeK8sPod, Arg: "//podspec-gpu.yaml"},
		}
		platform := labels.PickPlatform([]string{"ubuntu-latest"})
		assert.Equal(t, "node:22-bookworm", platform)
	})

	t.Run("mixed docker and k8spod labels", func(t *testing.T) {
		labels := Labels{
			{Name: "docker-runner", Schema: SchemeDocker, Arg: "//node:22"},
			{Name: "k8s-runner", Schema: SchemeK8sPod, Arg: "//podspec.yaml"},
		}
		assert.Equal(t, "node:22", labels.PickPlatform([]string{"docker-runner"}))
		assert.Equal(t, "k8spod", labels.PickPlatform([]string{"k8s-runner"}))
	})
}

func TestLabels_RequireDocker_WithK8s(t *testing.T) {
	t.Run("k8spod-only labels do not require docker", func(t *testing.T) {
		labels := Labels{
			{Name: "runner", Schema: SchemeK8sPod, Arg: "//podspec.yaml"},
		}
		assert.False(t, labels.RequireDocker())
	})

	t.Run("mixed labels with docker require docker", func(t *testing.T) {
		labels := Labels{
			{Name: "docker-runner", Schema: SchemeDocker, Arg: ArgDocker},
			{Name: "k8s-runner", Schema: SchemeK8sPod, Arg: "//podspec.yaml"},
		}
		assert.True(t, labels.RequireDocker())
	})
}
